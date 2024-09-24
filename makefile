PBUILDER_PKG = pbuilder-satisfydepends-dummy

GOPKG_PREFIX = github.com/linuxdeepin/lastore-daemon

GOPATH_DIR = gopath

pwd := ${shell pwd}
GoPath := GOPATH=${pwd}:${pwd}/vendor:${CURDIR}/${GOPATH_DIR}:${GOPATH}

GOBUILD = go build
GOTEST = go test -v
export GO111MODULE=off

export SECURITY_BUILD_OPTIONS = -fstack-protector-strong -D_FORTITY_SOURCE=1 -z noexecstack -pie -fPIC -z lazy

all:  build

TEST = \
	${GOPKG_PREFIX}/src/internal/system \
	${GOPKG_PREFIX}/src/internal/system/apt \
	${GOPKG_PREFIX}/src/internal/utils \
	${GOPKG_PREFIX}/src/internal/querydesktop \
	${GOPKG_PREFIX}/src/update-manager-daemon \
	${GOPKG_PREFIX}/src/update-manager-smartmirror \
	${GOPKG_PREFIX}/src/update-manager \
	${GOPKG_PREFIX}/src/update-manager-smartmirror-daemon

prepare:
	@mkdir -p out/bin
	@mkdir -p ${GOPATH_DIR}/src/$(dir ${GOPKG_PREFIX});
	@ln -snf ../../../.. ${GOPATH_DIR}/src/${GOPKG_PREFIX};

bin/update-manager-agent:src/update-manager-agent/*.c
	@mkdir -p bin
	gcc ${SECURITY_BUILD_OPTIONS} -W -Wall -D_GNU_SOURCE -o $@ $^ $(shell pkg-config --cflags --libs glib-2.0 libsystemd)

build: prepare bin/update-manager-agent
	${GoPath} ${GOBUILD} -o bin/update-manager-daemon ${GOBUILD_OPTIONS} ${GOPKG_PREFIX}/src/update-manager-daemon
	${GoPath} ${GOBUILD} -o bin/update-manager ${GOBUILD_OPTIONS} ${GOPKG_PREFIX}/src/update-manager
	${GoPath} ${GOBUILD} -o bin/update-manager-smartmirror ${GOBUILD_OPTIONS} ${GOPKG_PREFIX}/src/update-manager-smartmirror || echo "build failed, disable smartmirror support "
	${GoPath} ${GOBUILD} -o bin/update-manager-smartmirror-daemon ${GOBUILD_OPTIONS} ${GOPKG_PREFIX}/src/update-manager-smartmirror-daemon || echo "build failed, disable smartmirror support "
	${GoPath} ${GOBUILD} -o bin/update-manager-apt-clean ${GOBUILD_OPTIONS} ${GOPKG_PREFIX}/src/update-manager-apt-clean

fetch-base-metadata:
	./bin/update-manager update -r desktop -j applications -o var/lib/lastore/applications.json
	./bin/update-manager update -r desktop -j categories -o var/lib/lastore/categories.json
	./bin/update-manager update -r desktop -j mirrors -o var/lib/lastore/mirrors.json


test:
	NO_TEST_NETWORK=$(shell \
	if which dpkg >/dev/null;then \
		if dpkg -s ${PBUILDER_PKG} 2>/dev/null|grep 'Status:.*installed' >/dev/null;then \
			echo 1; \
		fi; \
	fi) \
	${GoPath} ${GOTEST} ${TEST}

test-coverage:
	env ${GoPath} go test -cover -v ./src/... | awk '$$1 ~ "^(ok|\\?)" {print $$2","$$5}' | sed "s:${CURDIR}::g" | sed 's/files\]/0\.0%/g' > coverage.csv


print_gopath:
	GOPATH="${pwd}:${pwd}/vendor:${GOPATH}"

install: gen_mo
	mkdir -p ${DESTDIR}${PREFIX}/usr/bin
	cp bin/update-manager-apt-clean ${DESTDIR}${PREFIX}/usr/bin/
	cp bin/update-manager ${DESTDIR}${PREFIX}/usr/bin/
	cp bin/update-manager-smartmirror ${DESTDIR}${PREFIX}/usr/bin/
	cp bin/update-manager-agent ${DESTDIR}${PREFIX}/usr/bin/
	mkdir -p ${DESTDIR}${PREFIX}/usr/libexec/deepin
	cp bin/update-manager-daemon ${DESTDIR}${PREFIX}/usr/libexec/deepin
	cp bin/update-manager-smartmirror-daemon ${DESTDIR}${PREFIX}/usr/libexec/deepin

	mkdir -p ${DESTDIR}${PREFIX}/usr && cp -rf usr ${DESTDIR}${PREFIX}/
	cp -rf etc ${DESTDIR}${PREFIX}/

	mkdir -p ${DESTDIR}${PREFIX}/var/lib/lastore/
	cp -rf var/lib/lastore/* ${DESTDIR}${PREFIX}/var/lib/lastore/
	cp -rf lib ${DESTDIR}${PREFIX}/

	mkdir -p ${DESTDIR}${PREFIX}/var/cache/lastore

update_pot:
	deepin-update-pot locale/locale_config.ini

gen_mo:
	deepin-generate-mo locale/locale_config.ini
	mkdir -p ${DESTDIR}${PREFIX}/usr/share/locale/
	cp -rf locale/mo/* ${DESTDIR}${PREFIX}/usr/share/locale/

	deepin-generate-mo locale_categories/locale_config.ini
	cp -rf locale_categories/mo/* ${DESTDIR}${PREFIX}/usr/share/locale/

gen-xml:
	qdbus --system org.deepin.dde.Lastore1 /org/deepin/dde/Lastore1 org.freedesktop.DBus.Introspectable.Introspect > usr/share/dbus-1/interfaces/org.deepin.dde.Lastore1.xml
	qdbus --system org.deepin.dde.Lastore1 /org/deepin/dde/Lastore1/Job1 org.freedesktop.DBus.Introspectable.Introspect > usr/share/dbus-1/interfaces/org.deepin.dde.Lastore1.Job.xml

build-deb:
	yes | debuild -us -uc

clean:
	rm -rf bin
	rm -rf pkg
	rm -rf vendor/pkg
	rm -rf vendor/bin

check_code_quality:
	${GoPath} go vet ./src/...
