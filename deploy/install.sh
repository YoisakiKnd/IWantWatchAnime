#!/bin/sh
# suzu 安装脚本（玩客云 / 任意 armv7 Linux）
#
# 干三件事：把二进制与配置摆到外接盘上、生成一份能直接跑的 config.toml、
# 装好 systemd 单元并起服务。重复执行是安全的：已有的配置不会被覆盖。
#
# 用法（在仓库根目录、已经 make armv7 之后）：
#   sudo deploy/install.sh
#   sudo SUZU_DATA=/mnt/sda1 deploy/install.sh        # 盘挂在别的地方
#   SUZU_NO_SYSTEMD=1 SUZU_PREFIX=/tmp/try deploy/install.sh   # 只摆文件，不碰 systemd
set -eu

DATA_DIR=${SUZU_DATA:-/mnt/usb}
BIN_SRC=${SUZU_BIN:-bin/suzu-linux-armv7}
UNIT_SRC=${SUZU_UNIT:-deploy/suzu.service}
TPL=${SUZU_TEMPLATE:-deploy/config.armv7.toml}
ROOT="$DATA_DIR/suzu"
DL_DIR="$DATA_DIR/downloads"
LIB_DIR="$DATA_DIR/library"
LISTEN=${SUZU_LISTEN:-127.0.0.1:8637}
USER_NAME=${SUZU_USER:-suzu}
NO_SYSTEMD=${SUZU_NO_SYSTEMD:-0}
PREFIX=${SUZU_PREFIX:-}

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

# ---- 0. 前置检查 ----------------------------------------------------------
if [ "$NO_SYSTEMD" = "0" ] && [ -z "$PREFIX" ] && [ "$(id -u)" != "0" ]; then
	die "要装成系统服务就得用 root 跑：sudo $0（或者 SUZU_NO_SYSTEMD=1 只摆文件）"
fi
[ -f "$BIN_SRC" ] || die "找不到二进制 $BIN_SRC，先在开发机上 make armv7，再把整个目录传过来"
[ -f "$TPL" ] || die "找不到配置模板 $TPL"

MACH=$(uname -m 2>/dev/null || echo unknown)
case "$MACH" in
	armv7l|armv6l|arm) : ;;
	*) warn "当前架构是 $MACH，这份二进制是给 armv7 的。确认无误就继续（脚本不拦）。" ;;
esac

# 外接盘挂上没？没挂就写下去会写进 eMMC —— 这正是要避免的事。
if [ "$NO_SYSTEMD" = "0" ] && [ -z "$PREFIX" ] && ! mountpoint -q "$DATA_DIR" 2>/dev/null; then
	die "$DATA_DIR 不是挂载点。外接盘没挂上时往下写，会写进 eMMC —— 请先挂盘，或改用 SUZU_DATA=<真实挂载点>"
fi

if [ -d "$DATA_DIR" ]; then
	FREE_KIB=$(df -Pk "$DATA_DIR" 2>/dev/null | awk 'NR==2{print $4}')
	if [ -n "${FREE_KIB:-}" ] && [ "$FREE_KIB" -lt 5242880 ] 2>/dev/null; then
		warn "$DATA_DIR 只剩 $((FREE_KIB / 1024)) MiB，入库会失败，先清点空间"
	fi
fi

# ---- 1. 目录 --------------------------------------------------------------
say "准备目录：$ROOT / $DL_DIR / $LIB_DIR"
mkdir -p "$ROOT/data" "$DL_DIR" "$LIB_DIR"

# ---- 2. 二进制 ------------------------------------------------------------
say "安装二进制 → $ROOT/suzu"
cp -f "$BIN_SRC" "$ROOT/suzu"
chmod 755 "$ROOT/suzu"
"$ROOT/suzu" -version >/dev/null 2>&1 || warn "二进制跑不起来（架构不对或文件损坏）"

# ---- 3. 配置 --------------------------------------------------------------
if [ -f "$ROOT/config.toml" ]; then
	say "已有配置，保留不动：$ROOT/config.toml"
else
	PW=${SUZU_PASSWORD:-$(tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 16 || true)}
	[ -n "$PW" ] || PW="suzu-$$-change-me"
	# 对局域网开放时用 0.0.0.0，否则本机。
	LAN_LISTEN=$(printf '%s' "$LISTEN" | sed 's/^127\.0\.0\.1/0.0.0.0/')
	if [ "$LISTEN" = "$LAN_LISTEN" ]; then LISTEN_DOC="$LISTEN"; else LISTEN_DOC="$LAN_LISTEN"; fi
	sed -e "s|@DATA_DIR@|$DATA_DIR|g" \
	    -e "s|@LISTEN@|$LISTEN|g" \
	    -e "s|@LISTEN_LAN@|$LAN_LISTEN|g" \
	    -e "s|@USER@|admin|g" \
	    -e "s|@PASSWORD@|$PW|g" \
	    "$TPL" > "$ROOT/config.toml"
	chmod 600 "$ROOT/config.toml"
	say "生成配置：$ROOT/config.toml"
	printf '\n    面板账号：admin\n    面板密码：\033[1;32m%s\033[0m   ← 记下来，config.toml 里也是这个\n\n' "$PW"
fi

# ---- 4. systemd -----------------------------------------------------------
if [ "$NO_SYSTEMD" = "1" ] || [ -n "$PREFIX" ]; then
	UNIT_DST=${PREFIX:-/tmp}/suzu.service
	mkdir -p "$(dirname "$UNIT_DST")"
	sed -e "s|/opt/suzu|$ROOT|g" \
	    -e "s|/mnt/usb/suzu|$ROOT|g" \
	    -e "s|/mnt/usb/downloads|$DL_DIR|g" \
	    -e "s|/mnt/usb/library|$LIB_DIR|g" \
	    -e "s|^User=suzu$|User=$(id -un)|" \
	    -e "s|^Group=suzu$|Group=$(id -gn)|" \
	    "$UNIT_SRC" > "$UNIT_DST"
	say "跳过 systemd（SUZU_NO_SYSTEMD/PREFIX）；单元文件渲染到 $UNIT_DST"
	say "手工起一次：$ROOT/suzu -config $ROOT/config.toml"
	exit 0
fi

if command -v getent >/dev/null 2>&1 && getent passwd "$USER_NAME" >/dev/null 2>&1; then
	say "用户 $USER_NAME 已存在"
else
	say "创建系统用户 $USER_NAME（不给登录、不给家目录）"
	useradd --system --no-create-home --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null \
		|| adduser -S -D -H -s /sbin/nologin "$USER_NAME" 2>/dev/null \
		|| warn "创建用户失败，请手工建一个 $USER_NAME"
fi

# 数据盘上的目录归 suzu 所有：unit 里是 suzu 用户在跑。
chown -R "$USER_NAME:$USER_NAME" "$ROOT" "$DL_DIR" "$LIB_DIR" 2>/dev/null || warn "chown 失败，确认 $USER_NAME 对这三个目录有写权限"

say "安装 systemd 单元 → /etc/systemd/system/suzu.service"
sed -e "s|/opt/suzu|$ROOT|g" \
    -e "s|/mnt/usb/suzu|$ROOT|g" \
    -e "s|/mnt/usb/downloads|$DL_DIR|g" \
    -e "s|/mnt/usb/library|$LIB_DIR|g" \
    "$UNIT_SRC" > /etc/systemd/system/suzu.service
systemctl daemon-reload
systemctl enable --now suzu

# ---- 5. 起来了吗 ----------------------------------------------------------
say "等服务起来…"
i=0
while [ $i -lt 15 ]; do
	if (command -v curl >/dev/null 2>&1 && curl -fsS "http://${LISTEN}/healthz" >/dev/null 2>&1) \
	   || (command -v wget >/dev/null 2>&1 && wget -qO- "http://${LISTEN}/healthz" >/dev/null 2>&1); then
		say "起来了 ✅"
		break
	fi
	i=$((i + 1)); sleep 1
done
[ $i -lt 15 ] || {
	warn "15 秒还没起来，看日志：journalctl -u suzu -n 50 --no-pager"
}

cat <<EOF

装完了。接下来：
  1. aria2 还没装就先装：sudo apt-get install -y aria2
     sudo systemctl enable --now aria2   # 参数见 deploy/README-armv7.md
  2. 在服务器本机打开面板：ssh -L 8637:127.0.0.1:8637 root@玩客云
     然后用笔记本浏览器访问 http://127.0.0.1:8637 （账号 admin / 上面那个密码）
  3. 面板 → 番剧库 → 搜番剧 → 勾字幕组 → 一键订阅
  4. 媒体库目录：$LIB_DIR  （指给 Jellyfin/Kodi 扫）

常用命令：
  journalctl -u suzu -f          看日志
  systemctl restart suzu         重启
  systemctl status suzu          看状态与内存

数据都在外接盘上：$ROOT（二进制+配置+数据库）、$DL_DIR（下载）、$LIB_DIR（媒体库）。
根文件系统只读，eMMC 不会被写。
EOF