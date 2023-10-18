package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"internal/system"
	"internal/system/apt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/godbus/dbus"
	"github.com/linuxdeepin/go-lib/gettext"
	debVersion "pault.ag/go/debian/version"
)

func prepareUpdateSource() {
	partialFilePaths := []string{
		"/var/lib/apt/lists/partial",
		"/var/lib/lastore/lists/partial",
		"/var/cache/apt/archives/partial",
		"/var/cache/lastore/archives/partial",
	}
	for _, partialFilePath := range partialFilePaths {
		infos, err := ioutil.ReadDir(partialFilePath)
		if err != nil {
			logger.Warning(err)
			continue
		}
		for _, info := range infos {
			err = os.RemoveAll(filepath.Join(partialFilePath, info.Name()))
			if err != nil {
				logger.Warning(err)
			}
		}
	}
	updateTokenConfigFile()
}

// updateSource 检查更新主要步骤:1.从更新平台获取数据并解析;2.依赖检查;3.apt update;4.最终可更新内容确定(模拟安装的方式);5.数据上报;
// 任务进度划分: 0-5%-10%-80%-90%-100%
func (m *Manager) updateSource(sender dbus.Sender, needNotify bool) (*Job, error) {
	var err error
	var environ map[string]string
	if !system.IsAuthorized() || !system.IsActiveCodeExist() {
		return nil, errors.New("not authorized, don't allow to exec update")
	}
	defer func() {
		if err == nil {
			err1 := m.config.UpdateLastCheckTime()
			if err1 != nil {
				logger.Warning(err1)
			}
			err1 = m.updateAutoCheckSystemUnit()
			if err != nil {
				logger.Warning(err)
			}
		}
	}()
	environ, err = makeEnvironWithSender(m, sender)
	if err != nil {
		return nil, err
	}
	prepareUpdateSource()
	m.jobManager.dispatch() // 解决 bug 59351问题（防止CreatJob获取到状态为end但是未被删除的job）
	var job *Job
	var isExist bool
	err = system.CustomSourceWrapper(system.AllCheckUpdate, func(path string, unref func()) error {
		m.do.Lock()
		defer m.do.Unlock()
		isExist, job, err = m.jobManager.CreateJob("", system.UpdateSourceJobType, nil, environ, nil)
		if err != nil {
			logger.Warningf("UpdateSource error: %v\n", err)
			if unref != nil {
				unref()
			}
			return err
		}
		if isExist {
			if unref != nil {
				unref()
			}
			logger.Info(JobExistError)
			return JobExistError
		}
		// 设置apt命令参数
		info, err := os.Stat(path)
		if err != nil {
			if unref != nil {
				unref()
			}
			return err
		}
		if info.IsDir() {
			job.option = map[string]string{
				"Dir::Etc::SourceList":  "/dev/null",
				"Dir::Etc::SourceParts": path,
			}
		} else {
			job.option = map[string]string{
				"Dir::Etc::SourceList":  path,
				"Dir::Etc::SourceParts": "/dev/null",
			}
		}
		job.subRetryHookFn = func(j *Job) {
			handleUpdateSourceFailed(j)
		}
		job.setPreHooks(map[string]func() error{
			string(system.RunningStatus): func() error {
				// 检查更新需要重置备份状态,主要是处理备份失败后再检查更新,会直接显示失败的场景
				m.statusManager.SetABStatus(system.AllCheckUpdate, system.NotBackup, system.NoABError)
				return nil
			},
			string(system.SucceedStatus): func() error {
				m.refreshUpdateInfos(true)
				job.setPropProgress(0.90)
				m.PropsMu.Lock()
				m.updateSourceOnce = true
				m.PropsMu.Unlock()
				if len(m.UpgradableApps) > 0 {
					m.updatePlatform.reportLog(updateStatusReport, true, "")
					m.updatePlatform.PostStatusMessage("")
					// m.installSpecialPackageSync("uos-release-note", job.option, environ)
					// 开启自动下载时触发自动下载,发自动下载通知,不发送可更新通知;
					// 关闭自动下载时,发可更新的通知;
					logger.Info("install uos-release-note done,update source succeed.")
					if !m.updater.AutoDownloadUpdates {
						// msg := gettext.Tr("New system edition available")
						msg := gettext.Tr("New version available!")
						action := []string{"view", gettext.Tr("View")}
						hints := map[string]dbus.Variant{"x-deepin-action-view": dbus.MakeVariant("dde-control-center,-m,update")}
						go m.sendNotify(updateNotifyShowOptional, 0, "preferences-system", "", msg, action, hints, system.NotifyExpireTimeoutDefault)
					}
				} else {
					m.updatePlatform.reportLog(updateStatusReport, false, "")
					m.updatePlatform.PostStatusMessage("")
				}
				job.setPropProgress(0.99)
				return nil
			},
			string(system.FailedStatus): func() error {
				// 网络问题检查更新失败和空间不足下载索引失败,需要发通知
				var errorContent = struct {
					ErrType   string
					ErrDetail string
				}{}
				err = json.Unmarshal([]byte(job.Description), &errorContent)
				if err == nil {
					if strings.Contains(errorContent.ErrType, string(system.ErrorFetchFailed)) || strings.Contains(errorContent.ErrType, string(system.ErrorIndexDownloadFailed)) {
						msg := gettext.Tr("Failed to check for updates. Please check your network.")
						action := []string{"view", gettext.Tr("View")}
						hints := map[string]dbus.Variant{"x-deepin-action-view": dbus.MakeVariant("dde-control-center,-m,network")}
						go m.sendNotify(updateNotifyShowOptional, 0, "preferences-system", "", msg, action, hints, system.NotifyExpireTimeoutDefault)
					}
					if strings.Contains(errorContent.ErrType, string(system.ErrorInsufficientSpace)) {
						msg := gettext.Tr("Failed to check for updates. Please clean up your disk first.")
						go m.sendNotify(updateNotifyShowOptional, 0, "preferences-system", "", msg, nil, nil, system.NotifyExpireTimeoutDefault)
					}
				}
				m.updatePlatform.reportLog(updateStatusReport, false, job.Description)
				m.updatePlatform.PostStatusMessage("")
				return nil
			},
			string(system.EndStatus): func() error {
				// wrapper的资源释放
				if unref != nil {
					unref()
				}
				return nil
			},
		})
		job.setAfterHooks(map[string]func() error{
			string(system.RunningStatus): func() error {
				job.setPropProgress(0.01)
				// 检查任务开始后,从更新平台获取仓库、更新注记等信息 TODO 可能不再需要
				// 从更新平台获取数据:系统更新和安全更新流程都包含
				if !m.updatePlatform.genUpdatePolicyByToken() {
					job.retry = 0
					m.updatePlatform.PostStatusMessage("")
					return errors.New("failed to get update policy by token")
				}
				err = m.updatePlatform.UpdateAllPlatformDataSync()
				if err != nil {
					logger.Warning(err)
					job.retry = 0
					m.updatePlatform.PostStatusMessage("")
					return fmt.Errorf("failed to get update info by update platform: %v", err)
				}
				m.updater.setPropUpdateTarget(m.updatePlatform.getUpdateTarget())

				// 从更新平台获取数据并处理完成后,进度更新到6%
				job.setPropProgress(0.06)

				// 系统工具检查依赖关系
				// err = m.updateApi.CheckSystem("", "")
				// if err != nil {
				// 	logger.Warning(err)
				// 	return err
				// }
				// // 检查完成后进度更新到10%
				// job.setPropProgress(0.10)
				return nil
			},
			string(system.SucceedStatus): func() error {
				job.setPropProgress(1.00)
				return nil
			},
		})

		if err = m.jobManager.addJob(job); err != nil {
			logger.Warning(err)
			if unref != nil {
				unref()
			}
			return err
		}
		return nil
	})
	if err != nil && !errors.Is(err, JobExistError) { // exist的err无需返回
		logger.Warning(err)
		return nil, err
	}
	return job, nil
}

// 生成系统更新内容和安全更新内容
func (m *Manager) generateUpdateInfo(platFormPackageList map[string]system.UpgradeInfo) (error, error) {
	propPkgMap := make(map[string][]string) // updater的ClassifiedUpdatablePackages用
	var systemErr error = nil
	var securityErr error = nil
	var systemPackageList map[string]system.UpgradeInfo
	var securityPackageList map[string]system.UpgradeInfo
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		systemPackageList, systemErr = getSystemUpdatePackageList(platFormPackageList)
		if systemErr == nil && systemPackageList != nil {
			var packageList []string
			for k, v := range systemPackageList {
				packageList = append(packageList, fmt.Sprintf("%v=%v", k, v.LastVersion))
			}
			propPkgMap[system.SystemUpdate.JobType()] = packageList
		}
		wg.Done()
	}()
	wg.Add(1)
	go func() {
		securityPackageList, securityErr = getSecurityUpdatePackageList()
		if securityErr == nil && securityPackageList != nil {
			var packageList []string
			for k, v := range securityPackageList {
				packageList = append(packageList, fmt.Sprintf("%v=%v", k, v.LastVersion))
			}
			propPkgMap[system.SecurityUpdate.JobType()] = packageList
		}
		wg.Done()
	}()
	wg.Wait()
	m.updater.setClassifiedUpdatablePackages(propPkgMap)
	return systemErr, securityErr
}

func getSystemUpdatePackageList(platFormPackageList map[string]system.UpgradeInfo) (map[string]system.UpgradeInfo, error) {
	// 从平台更新列表获取需要更新或者安装的包
	cache, err := loadPkgStatusVersion()
	if err != nil {
		logger.Warning(err)
		return nil, err
	}
	var needInstallPackage []string
	for _, pkg := range platFormPackageList {
		status, ok := cache[pkg.Package]
		if !ok {
			// 系统没有该包的情况
			needInstallPackage = append(needInstallPackage, fmt.Sprintf("%v=%v", pkg.Package, pkg.LastVersion))
			continue
		}
		if compareVersionsGe(status.version, pkg.LastVersion) {
			// 包已安装版本>=平台下发包的版本 ：可以忽略,无需安装
			continue
		} else {
			// 包已安装版本 < 平台下发包的版本
			// return nil, fmt.Errorf("package %v_%v obtained from the platform can not install in current OS", pkg.Package, pkg.LastVersion)
			needInstallPackage = append(needInstallPackage, fmt.Sprintf("%v=%v", pkg.Package, pkg.LastVersion))
		}
	}
	return apt.GenOnlineUpdatePackagesByEmulateInstall(needInstallPackage, []string{
		"-o", fmt.Sprintf("Dir::Etc::sourcelist=%v", system.GetCategorySourceMap()[system.SystemUpdate]),
		"-o", "Dir::Etc::SourceParts=/dev/null",
		"-o", "Dir::Etc::preferences=/dev/null", // 系统更新仓库来自更新平台，为了不收本地优先级配置影响，覆盖本地优先级配置
		"-o", "Dir::Etc::PreferencesParts=/dev/null",
	})
}

// 通过 sudo apt install -s dde-api=5.5.35-1 startdde=5.10.12.4-1 dde-api-dbgsym=5.5.35-1 获取更新列表
// func getSystemUpdatePackageList(platFormPackageList map[string]system.UpgradeInfo) ([]string, error) {
// 	platFormPackageListInner := platFormPackageList
// 	// 拿到系统仓库的可更新包列表
// 	var endNameListWithVersion []string
// 	pkgList, err := listDistUpgradePackages(system.SystemUpdate) // 仓库检查出所有可以升级的包
// 	if err != nil {
// 		if os.IsNotExist(err) { // 该类型源文件不存在时
// 			logger.Info(err)
// 		} else {
// 			logger.Warningf("failed to list %v upgradable package %v", system.SystemUpdate.JobType(), err)
// 		}
// 		return nil, err
// 	}
// 	// 和更新平台的数据比对,找到允许升级的包，此处不会判断版本
// 	for _, pkg := range pkgList {
// 		info, ok := platFormPackageListInner[pkg]
// 		if ok {
// 			endNameListWithVersion = append(endNameListWithVersion, fmt.Sprintf("%v=%v", info.Package, info.LastVersion))
// 		}
// 		delete(platFormPackageListInner, pkg)
// 	}
// 	// 证明从平台获取的包还有未匹配上的(可能是已经升级了，或者依赖关系导致，或者仓库问题导致无法升级)
// 	if len(platFormPackageListInner) > 0 {
// 		// 获取本地所有包状态
// 		cache, err := loadPkgStatusVersion()
// 		if err != nil {
// 			logger.Warning(err)
// 			return nil, err
// 		}
// 		for _, pkg := range platFormPackageListInner {
// 			status, ok := cache[pkg.Package]
// 			if !ok {
// 				// 系统没有该包的情况
// 				return nil, fmt.Errorf("package %v obtained from the platform can not install in current OS", pkg.Package)
// 			}
// 			if compareVersionsGe(status.version, pkg.LastVersion) {
// 				// 包已安装版本>=平台下发包的版本 ：可以忽略,无需安装
// 				continue
// 			} else {
// 				// 包已安装版本 < 平台下发包的版本 ：环境存在问题,需要上报
// 				return nil, fmt.Errorf("package %v_%v obtained from the platform can not install in current OS", pkg.Package, pkg.LastVersion)
// 			}
// 		}
// 	}
// 	// 判断匹配到的包版本是否存在
// 	if !checkDebExistWithVersion(endNameListWithVersion) {
// 		return nil, errors.New("apt show err ,some package can not install")
// 	}
// 	// 补充强依赖包，eg：需要安装startdde，但是当前系统 startdde-dbgsym 也安装了，那么需要在下一步进行补充 startdde-dbgsym，这一步补充不需要带版本号
// 	newInstallList, err := apt.ListInstallPackages(endNameListWithVersion)
// 	if err != nil {
// 		logger.Warning(err)
// 		return nil, err
// 	}
// 	return append(endNameListWithVersion, newInstallList...), nil
// }

func getSecurityUpdatePackageList() (map[string]system.UpgradeInfo, error) {
	pkgList, err := listDistUpgradePackages(system.SecurityUpdate) // 仓库检查出所有可以升级的包
	if err != nil {
		if os.IsNotExist(err) { // 该类型源文件不存在时
			logger.Info(err)
			return nil, nil // 该错误无需返回
		} else {
			logger.Warningf("failed to list %v upgradable package %v", system.SystemUpdate.JobType(), err)
			return nil, err
		}
	}
	return apt.GenOnlineUpdatePackagesByEmulateInstall(pkgList, []string{
		"-o", fmt.Sprintf("Dir::Etc::sourcelist=%v", system.GetCategorySourceMap()[system.SecurityUpdate]),
		"-o", "Dir::Etc::SourceParts=/dev/null",
	})
}

// 判断包对应版本是否存在
func checkDebExistWithVersion(pkgList []string) bool {
	args := strings.Join(pkgList, " ")
	args = "apt show " + args
	cmd := exec.Command("/bin/sh", "-c", args)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		logger.Warning(outBuf.String())
		logger.Warning(errBuf.String())
	}
	return err == nil
}

type statusVersion struct {
	status  string
	version string
}

func loadPkgStatusVersion() (map[string]statusVersion, error) {
	out, err := exec.Command("dpkg-query", "-f", "${Package} ${db:Status-Abbrev} ${Version}\n", "-W").Output() // #nosec G204
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(out)
	scanner := bufio.NewScanner(reader)
	result := make(map[string]statusVersion)
	for scanner.Scan() {
		line := scanner.Bytes()
		fields := bytes.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pkg := string(fields[0])
		status := string(fields[1])
		version := string(fields[2])
		result[pkg] = statusVersion{
			status:  status,
			version: version,
		}
	}
	err = scanner.Err()
	if err != nil {
		return nil, err
	}
	return result, nil
}

func compareVersionsGe(ver1, ver2 string) bool {
	gt, err := compareVersionsGeFast(ver1, ver2)
	if err != nil {
		return compareVersionsGeDpkg(ver1, ver2)
	}
	return gt
}

func compareVersionsGeFast(ver1, ver2 string) (bool, error) {
	v1, err := debVersion.Parse(ver1)
	if err != nil {
		return false, err
	}
	v2, err := debVersion.Parse(ver2)
	if err != nil {
		return false, err
	}
	return debVersion.Compare(v1, v2) >= 0, nil
}

func compareVersionsGeDpkg(ver1, ver2 string) bool {
	err := exec.Command("dpkg", "--compare-versions", "--", ver1, "ge", ver2).Run() // #nosec G204
	return err == nil
}

func listDistUpgradePackages(updateType system.UpdateType) ([]string, error) {
	sourcePath := system.GetCategorySourceMap()[updateType]
	return apt.ListDistUpgradePackages(sourcePath, nil)
}

func (m *Manager) refreshUpdateInfos(sync bool) {
	// 检查更新时,同步修改canUpgrade状态;检查更新时需要同步操作
	if sync {
		systemErr, securityErr := m.generateUpdateInfo(m.updatePlatform.GetSystemMeta())
		if systemErr != nil {
			m.updatePlatform.PostStatusMessage("")
			logger.Warning(systemErr)
		}
		if securityErr != nil {
			m.updatePlatform.PostStatusMessage("")
			logger.Warning(securityErr)
		}
		m.statusManager.UpdateModeAllStatusBySize()
		m.statusManager.UpdateCheckCanUpgradeByEachStatus()
	} else {
		go func() {
			m.statusManager.UpdateModeAllStatusBySize()
			m.statusManager.UpdateCheckCanUpgradeByEachStatus()
		}()
	}
	m.updateUpdatableProp(m.updater.ClassifiedUpdatablePackages)
	if m.updater.AutoDownloadUpdates && len(m.updater.UpdatablePackages) > 0 && sync && !m.updater.getIdleDownloadEnabled() {
		logger.Info("auto download updates")
		go func() {
			m.inhibitAutoQuitCountAdd()
			_, err := m.PrepareDistUpgrade(dbus.Sender(m.service.Conn().Names()[0]))
			if err != nil {
				logger.Error("failed to prepare dist-upgrade:", err)
			}
			m.inhibitAutoQuitCountSub()
		}()
	}
}

func (m *Manager) updateUpdatableProp(infosMap map[string][]string) {
	m.PropsMu.RLock()
	updateType := m.UpdateMode
	m.PropsMu.RUnlock()
	filterInfos := getFilterPackages(infosMap, updateType)
	m.updatableApps(filterInfos) // Manager的UpgradableApps实际为可更新的包,而非应用;
	m.updater.setUpdatablePackages(filterInfos)
	m.updater.updateUpdatableApps()
}

func (m *Manager) ensureUpdateSourceOnce() {
	m.PropsMu.Lock()
	updateOnce := m.updateSourceOnce
	m.PropsMu.Unlock()

	if updateOnce {
		return
	}

	_, err := m.updateSource(dbus.Sender(m.service.Conn().Names()[0]), false)
	if err != nil {
		logger.Warning(err)
		return
	}
}

// retry次数为1
// 默认检查为 AllCheckUpdate
// 重试检查为 AllInstallUpdate
func handleUpdateSourceFailed(j *Job) {
	retry := j.retry
	if retry > 1 {
		retry = 1
		j.retry = retry
	}
	retryMap := map[int]system.UpdateType{
		1: system.AllInstallUpdate,
	}
	updateType := retryMap[retry]
	err := system.CustomSourceWrapper(updateType, func(path string, unref func()) error {
		// 重新设置apt命令参数
		info, err := os.Stat(path)
		if err != nil {
			if unref != nil {
				unref()
			}
			return err
		}
		if info.IsDir() {
			j.option = map[string]string{
				"Dir::Etc::SourceList":  "/dev/null",
				"Dir::Etc::SourceParts": path,
			}
		} else {
			j.option = map[string]string{
				"Dir::Etc::SourceList":  path,
				"Dir::Etc::SourceParts": "/dev/null",
			}
		}
		j.wrapPreHooks(map[string]func() error{
			string(system.EndStatus): func() error {
				if unref != nil {
					unref()
				}
				return nil
			},
		})
		return nil
	})
	if err != nil {
		logger.Warning(err)
	}
}
