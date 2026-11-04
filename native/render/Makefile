# SPDX-License-Identifier: GPL-2.0-or-later
# 默认静态链接固定版本与补丁，不允许误用系统未修复的 Gutenprint。
CC ?= cc
PYTHON ?= python3
GUTENPRINT_PREFIX ?= /opt/opencanon/gutenprint
GUTENPRINT_BUILD ?= $(CURDIR)/.gutenprint-build
GUTENPRINT_LIB = $(GUTENPRINT_PREFIX)/lib/libgutenprint.a
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
DOCDIR ?= $(PREFIX)/share/doc/opencanon-render
CPPFLAGS += -I$(GUTENPRINT_PREFIX)/include
CFLAGS ?= -O2 -g
CFLAGS += -std=c11 -Wall -Wextra -Werror
LDLIBS += $(GUTENPRINT_LIB) -lm

.PHONY: all install clean test gutenprint source-docs
all: opencanon-render source-docs

gutenprint: $(GUTENPRINT_LIB)

$(GUTENPRINT_LIB): gutenprint-buffer.patch build-gutenprint.sh
	sh ./build-gutenprint.sh "$(GUTENPRINT_BUILD)" "$(GUTENPRINT_PREFIX)"

opencanon-render: opencanon-render.c $(GUTENPRINT_LIB)
	$(CC) $(CPPFLAGS) $(CFLAGS) $(LDFLAGS) -o $@ $< $(LDLIBS)

# 增量构建也同步对应源码，避免分发旧辅助程序源码或旧许可说明。
source-docs: $(GUTENPRINT_LIB)
	install -d "$(GUTENPRINT_PREFIX)/share/doc/opencanon-render"
	install -m 0644 COPYING NOTICE opencanon-render.c Makefile build-gutenprint.sh gutenprint-buffer.patch regression.py "$(GUTENPRINT_PREFIX)/share/doc/opencanon-render/"

test: all
	$(PYTHON) regression.py ./opencanon-render

install: all
	install -d "$(DESTDIR)$(BINDIR)" "$(DESTDIR)$(DOCDIR)"
	install -m 0755 opencanon-render "$(DESTDIR)$(BINDIR)/opencanon-render"
	install -m 0644 COPYING NOTICE opencanon-render.c Makefile build-gutenprint.sh gutenprint-buffer.patch regression.py "$(DESTDIR)$(DOCDIR)/"

clean:
	rm -f opencanon-render
