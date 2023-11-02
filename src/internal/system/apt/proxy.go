// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package apt

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"internal/system"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type APTSystem struct {
	CmdSet    map[string]*system.Command
	Indicator system.Indicator
}

func NewSystem(systemSourceList []string, nonUnknownList []string, otherList []string) system.System {
	apt := New(systemSourceList, nonUnknownList, otherList)
	return &apt
}

func New(systemSourceList []string, nonUnknownList []string, otherList []string) APTSystem {
	p := APTSystem{
		CmdSet: make(map[string]*system.Command),
	}
	WaitDpkgLockRelease()
	_ = exec.Command("/var/lib/lastore/scripts/build_safecache.sh").Run() // TODO
	p.initSource(systemSourceList, nonUnknownList, otherList)
	return p
}

func parseProgressField(v string) (float64, error) {
	progress, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return -1, fmt.Errorf("unknown progress value: %q", v)
	}
	return progress, nil
}

func parseProgressInfo(id, line string) (system.JobProgressInfo, error) {
	fs := strings.SplitN(line, ":", 4)
	if len(fs) != 4 {
		return system.JobProgressInfo{JobId: id}, fmt.Errorf("Invlaid Progress line:%q", line)
	}

	progress, err := parseProgressField(fs[2])
	if err != nil {
		return system.JobProgressInfo{JobId: id}, err
	}
	description := strings.TrimSpace(fs[3])

	var status system.Status
	var cancelable = true

	infoType := fs[0]

	switch infoType {
	case "dummy":
		status = system.Status(fs[1])
	case "dlstatus":
		progress = progress / 100.0
		status = system.RunningStatus
	case "pmstatus":
		progress = progress / 100.0
		status = system.RunningStatus
		cancelable = false
	case "pmerror":
		progress = -1
		status = system.FailedStatus

	default:
		//	case "pmconffile", "media-change":
		return system.JobProgressInfo{JobId: id},
			fmt.Errorf("W: unknow status:%q", line)

	}

	return system.JobProgressInfo{
		JobId:       id,
		Progress:    progress,
		Description: description,
		Status:      status,
		Cancelable:  cancelable,
	}, nil
}

func (p *APTSystem) AttachIndicator(f system.Indicator) {
	p.Indicator = f
}

func WaitDpkgLockRelease() {
	for {
		msg, wait := checkLock("/var/lib/dpkg/lock")
		if wait {
			logger.Warningf("Wait 5s for unlock\n\"%s\" \n at %v\n",
				msg, time.Now())
			time.Sleep(time.Second * 5)
			continue
		}

		msg, wait = checkLock("/var/lib/dpkg/lock-frontend")
		if wait {
			logger.Warningf("Wait 5s for unlock\n\"%s\" \n at %v\n",
				msg, time.Now())
			time.Sleep(time.Second * 5)
			continue
		}

		return
	}
}

func checkLock(p string) (string, bool) {
	// #nosec G304
	file, err := os.Open(p)
	if err != nil {
		logger.Warningf("error opening %q: %v", p, err)
		return "", false
	}
	defer func() {
		_ = file.Close()
	}()

	flockT := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: io.SeekStart,
		Start:  0,
		Len:    0,
		Pid:    0,
	}
	err = syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &flockT)
	if err != nil {
		logger.Warningf("unable to check file %q lock status: %s", p, err)
		return p, true
	}

	if flockT.Type == syscall.F_WRLCK {
		return p, true
	}

	return "", false
}

func parsePkgSystemError(out, err []byte) error {
	if len(err) == 0 {
		return nil
	}
	switch {
	case bytes.Contains(err, []byte("dpkg was interrupted")):
		return &system.PkgSystemError{
			Type: system.ErrTypeDpkgInterrupted,
		}

	case bytes.Contains(err, []byte("Unmet dependencies")):
		var detail string
		idx := bytes.Index(out,
			[]byte("The following packages have unmet dependencies:"))
		if idx == -1 {
			// not found
			detail = string(out)
		} else {
			detail = string(out[idx:])
		}

		return &system.PkgSystemError{
			Type:   system.ErrTypeDependenciesBroken,
			Detail: detail,
		}

	case bytes.Contains(err, []byte("The list of sources could not be read")):
		detail := string(err)
		return &system.PkgSystemError{
			Type:   system.ErrTypeInvalidSourcesList,
			Detail: detail,
		}

	default:
		detail := string(err)
		return &system.PkgSystemError{
			Type:   system.ErrTypeUnknown,
			Detail: detail,
		}
	}
}

func CheckPkgSystemError(lock bool) error {
	args := []string{"check"}
	if !lock {
		// without locking, it can only check for dependencies broken
		args = append(args, "-o", "Debug::NoLocking=1")
	}

	cmd := exec.Command("apt-get", args...)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return nil
	}
	return parsePkgSystemError(outBuf.Bytes(), errBuf.Bytes())
}

func safeStart(c *system.Command) error {
	args := c.Cmd.Args
	// add -s option
	args = append([]string{"-s"}, args[1:]...)
	cmd := exec.Command("apt-get", args...) // #nosec G204

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// perform apt-get action simulate
	err := cmd.Start()
	if err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			jobErr := parseJobError(stderr.String(), stdout.String())
			c.IndicateFailed(jobErr.Type, jobErr.Detail, false)
			return
		}

		// cmd run ok
		// check rm dde?
		if bytes.Contains(stdout.Bytes(), []byte("Remv dde ")) {
			c.IndicateFailed("removeDDE", "", true)
			return
		}

		// really perform apt-get action
		err = c.Start()
		if err != nil {
			c.IndicateFailed(system.ErrorUnknown,
				"apt-get start failed: "+err.Error(), false)
		}
	}()
	return nil
}

func OptionToArgs(options map[string]string) []string {
	var args []string
	for key, value := range options { // apt 命令执行参数
		args = append(args, "-o")
		args = append(args, fmt.Sprintf("%v=%v", key, value))
	}
	return args
}

func (p *APTSystem) DownloadPackages(jobId string, packages []string, environ map[string]string, args map[string]string) error {
	err := CheckPkgSystemError(false)
	if err != nil {
		return err
	}
	c := newAPTCommand(p, jobId, system.DownloadJobType, p.Indicator, append(packages, OptionToArgs(args)...))
	c.SetEnv(environ)
	return c.Start()
}

func (p *APTSystem) DownloadSource(jobId string, environ map[string]string, args map[string]string) error {
	// 无需检查依赖错误
	/*
		err := CheckPkgSystemError(false)
		if err != nil {
			return err
		}
	*/

	c := newAPTCommand(p, jobId, system.PrepareDistUpgradeJobType, p.Indicator, OptionToArgs(args))
	c.SetEnv(environ)
	return c.Start()
}

func (p *APTSystem) Remove(jobId string, packages []string, environ map[string]string) error {
	WaitDpkgLockRelease()
	err := CheckPkgSystemError(true)
	if err != nil {
		return err
	}

	c := newAPTCommand(p, jobId, system.RemoveJobType, p.Indicator, packages)
	c.SetEnv(environ)
	return safeStart(c)
}

func (p *APTSystem) Install(jobId string, packages []string, environ map[string]string, args map[string]string) error {
	WaitDpkgLockRelease()
	err := CheckPkgSystemError(true)
	if err != nil {
		return err
	}
	c := newAPTCommand(p, jobId, system.InstallJobType, p.Indicator, append(packages, OptionToArgs(args)...))
	c.SetEnv(environ)
	return safeStart(c)
}

func (p *APTSystem) DistUpgrade(jobId string, environ map[string]string, args map[string]string) error {
	WaitDpkgLockRelease()
	err := CheckPkgSystemError(true)
	if err != nil {
		// 无需处理依赖错误,在获取可更新包时,使用dist-upgrade -d命令获取,就会报错了
		var e *system.PkgSystemError
		ok := errors.As(err, &e)
		if !ok || (ok && e.Type != system.ErrTypeDependenciesBroken) {
			return err
		}
	}
	c := newAPTCommand(p, jobId, system.DistUpgradeJobType, p.Indicator, OptionToArgs(args))
	c.SetEnv(environ)
	return safeStart(c)
}

func (p *APTSystem) UpdateSource(jobId string, environ map[string]string, args map[string]string) error {
	c := newAPTCommand(p, jobId, system.UpdateSourceJobType, p.Indicator, OptionToArgs(args))
	c.AtExitFn = func() bool {
		// 无网络时检查更新失败,exitCode为0,空间不足(不确定exit code)导致需要特殊处理
		if c.ExitCode == system.ExitSuccess && bytes.Contains(c.Stderr.Bytes(), []byte("Some index files failed to download")) {
			if bytes.Contains(c.Stderr.Bytes(), []byte("No space left on device")) {
				c.IndicateFailed(system.ErrorInsufficientSpace, c.Stderr.String(), false)
			} else {
				c.IndicateFailed(system.ErrorIndexDownloadFailed, c.Stderr.String(), false)
			}
			return true
		}
		return false
	}
	c.SetEnv(environ)
	return c.Start()
}

func (p *APTSystem) Clean(jobId string) error {
	c := newAPTCommand(p, jobId, system.CleanJobType, p.Indicator, nil)
	return c.Start()
}

func (p *APTSystem) Abort(jobId string) error {
	if c := p.FindCMD(jobId); c != nil {
		return c.Abort()
	}
	return system.NotFoundError("abort " + jobId)
}

func (p *APTSystem) AbortWithFailed(jobId string) error {
	if c := p.FindCMD(jobId); c != nil {
		return c.AbortWithFailed()
	}
	return system.NotFoundError("abort " + jobId)
}

func (p *APTSystem) FixError(jobId string, errType string, environ map[string]string, args map[string]string) error {
	WaitDpkgLockRelease()
	c := newAPTCommand(p, jobId, system.FixErrorJobType, p.Indicator, append([]string{errType}, OptionToArgs(args)...))
	c.SetEnv(environ)
	if errType == system.ErrTypeDependenciesBroken { // 修复依赖错误的时候，会有需要卸载dde的情况，因此需要用safeStart来进行处理
		return safeStart(c)
	}
	return c.Start()
}

func (p *APTSystem) CheckSystem(jobId string, checkType string, environ map[string]string, cmdArgs map[string]string) error {
	return nil
}

func (p *APTSystem) initSource(systemSourceList []string, nonUnknownList []string, otherList []string) {
	// apt初始化时执行一次，避免其他apt操作过程中删改软链接导致数据异常
	err := system.UpdateUnknownSourceDir(nonUnknownList)
	if err != nil {
		logger.Warning(err)
	}

	err = system.UpdateSystemSourceDir(systemSourceList)
	if err != nil {
		logger.Warning(err)
	}
	err = system.UpdateOtherSystemSourceDir(otherList)
	if err != nil {
		logger.Warning(err)
	}
	err = ioutil.WriteFile(system.PlatFormSourceFile, []byte{}, 0644)
	if err != nil {
		logger.Warning(err)
	}
}

func ListInstallPackages(packages []string) ([]string, error) {
	args := []string{
		"-c", system.LastoreAptV2CommonConfPath,
		"install", "-s",
		"-o", "Debug::NoLocking=1",
	}
	args = append(args, packages...)
	cmd := exec.Command("apt-get", args...) // #nosec G204
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	// NOTE: 这里不能使用命令的退出码来判断，因为 --assume-no 会让命令的退出码为 1
	_ = cmd.Run()

	const newInstalled = "The following additional packages will be installed:"
	if bytes.Contains(outBuf.Bytes(), []byte(newInstalled)) {
		p := parseAptShowList(bytes.NewReader(outBuf.Bytes()), newInstalled)
		return p, nil
	}

	err := parsePkgSystemError(outBuf.Bytes(), errBuf.Bytes())
	return nil, err
}

var _installRegex = regexp.MustCompile(`Inst (.*) \[.*] \(([^ ]+) .*\)`)
var _installRegex2 = regexp.MustCompile(`Inst (.*) \(([^ ]+) .*\)`)

// GenOnlineUpdatePackagesByEmulateInstall option 需要带上仓库参数
func GenOnlineUpdatePackagesByEmulateInstall(packages []string, option []string) (map[string]system.PackageInfo, error) {
	allPackages := make(map[string]system.PackageInfo)
	args := []string{
		"install", "-s",
		"-c", system.LastoreAptV2CommonConfPath,
		"-o", "Debug::NoLocking=1",
	}
	args = append(args, option...)
	args = append(args, packages...)
	cmd := exec.Command("apt-get", args...) // #nosec G204
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		logger.Warning(errBuf.String())
		return nil, err
	}
	const upgraded = "The following packages will be upgraded:"
	const newInstalled = "The following NEW packages will be installed:"
	if bytes.Contains(outBuf.Bytes(), []byte(upgraded)) ||
		bytes.Contains(outBuf.Bytes(), []byte(newInstalled)) {
		// 证明平台要求包可以安装
		allLine := strings.Split(outBuf.String(), "\n")
		for _, line := range allLine {
			matches := _installRegex.FindStringSubmatch(line)
			if len(matches) < 2 {
				matches = _installRegex2.FindStringSubmatch(line)
			}
			if len(matches) > 2 {
				allPackages[matches[1]] = system.PackageInfo{
					Name:    matches[1],
					Version: matches[2],
				}
				continue
			}
		}
	}
	return allPackages, nil
}

// ListDistUpgradePackages return the pkgs from apt dist-upgrade
// NOTE: the result strim the arch suffix
func ListDistUpgradePackages(sourcePath string, option []string) ([]string, error) {
	args := []string{
		"-c", system.LastoreAptV2CommonConfPath,
		"dist-upgrade", "--assume-no",
		"-o", "Debug::NoLocking=1",
	}
	args = append(args, option...)
	if info, err := os.Stat(sourcePath); err == nil {
		if info.IsDir() {
			args = append(args, "-o", "Dir::Etc::SourceList=/dev/null")
			args = append(args, "-o", "Dir::Etc::SourceParts="+sourcePath)
		} else {
			args = append(args, "-o", "Dir::Etc::SourceList="+sourcePath)
			args = append(args, "-o", "Dir::Etc::SourceParts=/dev/null")
		}
	} else {
		return nil, err
	}

	cmd := exec.Command("apt-get", args...) // #nosec G204
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	// NOTE: 这里不能使用命令的退出码来判断，因为 --assume-no 会让命令的退出码为 1
	_ = cmd.Run()

	const upgraded = "The following packages will be upgraded:"
	const newInstalled = "The following NEW packages will be installed:"
	if bytes.Contains(outBuf.Bytes(), []byte(upgraded)) ||
		bytes.Contains(outBuf.Bytes(), []byte(newInstalled)) {

		p := parseAptShowList(bytes.NewReader(outBuf.Bytes()), upgraded)
		p = append(p, parseAptShowList(bytes.NewReader(outBuf.Bytes()), newInstalled)...)
		return p, nil
	}

	err := parsePkgSystemError(outBuf.Bytes(), errBuf.Bytes())
	return nil, err
}

func parseAptShowList(r io.Reader, title string) []string {
	buf := bufio.NewReader(r)

	var p []string

	var line string
	in := false

	var err error
	for err == nil {
		line, err = buf.ReadString('\n')
		if strings.TrimSpace(title) == strings.TrimSpace(line) {
			in = true
			continue
		}

		if !in {
			continue
		}

		if !strings.HasPrefix(line, " ") {
			break
		}

		for _, f := range strings.Fields(line) {
			p = append(p, strings.Split(f, ":")[0])
		}
	}

	return p
}
