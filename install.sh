#!/bin/sh
# Interactive installer for the latest RemoteMAC GitHub Release.
# Requires a controlling terminal; installs a verified prebuilt binary.
set -eu
umask 077

DEFAULT_REPO=ynhuu/RemoteMAC

die() { printf '%s\n' "错误：$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
usage() {
  cat <<'EOF'
用法：sh install.sh install [--version vX.Y.Z] [--repo owner/repository]
      sh install.sh uninstall
      sh install.sh --help

默认从 ynhuu/RemoteMAC 的 latest Release 下载当前 Mac 架构的 ZIP 资产，
校验 SHA256SUMS 后安装；不会从本地源码构建，也不需要 Go、Node.js 或 pnpm。
安装会为当前登录用户创建 LaunchAgent；服务在登录后于后台运行，不会打开 Terminal 窗口。
二进制安装到 /usr/local/bin/remotemac，需要管理员授权；口令和配置保存在当前用户目录。
EOF
}

action=${1:---help}
case "$action" in
  --help|-h) usage; exit 0 ;;
  install|uninstall) shift ;;
  *) usage; exit 1 ;;
esac

repo=$DEFAULT_REPO
version=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo)
      [ "$#" -ge 2 ] || die '--repo 缺少参数'
      repo=$2
      shift 2
      ;;
    --version)
      [ "$#" -ge 2 ] || die '--version 缺少参数'
      version=$2
      shift 2
      ;;
    *) die "未知参数：$1" ;;
  esac
done

if [ "$action" = uninstall ] && { [ "$repo" != "$DEFAULT_REPO" ] || [ -n "$version" ]; }; then
  die 'uninstall 不接受 --repo 或 --version'
fi

[ "$(uname -s)" = Darwin ] || die '安装器仅支持 macOS'

# Keep per-user configuration owned by the logged-in user when invoked via sudo.
if [ "$(id -u)" -eq 0 ]; then
  if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
    set -- "$action"
    [ "$repo" = "$DEFAULT_REPO" ] || set -- "$@" --repo "$repo"
    [ -z "$version" ] || set -- "$@" --version "$version"
    exec sudo -H -u "$SUDO_USER" /bin/sh "$0" "$@"
  fi
  die '请从登录用户运行安装器；安装系统二进制时脚本会单独请求管理员授权'
fi

home=$HOME
uid=$(id -u)
user=$(id -un)
label=com.remotemac.server
binary=/usr/local/bin/remotemac
base=$home/.local/share/remotemac
config=$home/.config/remotemac
unit=$home/Library/LaunchAgents/$label.plist
launch_agents=$home/Library/LaunchAgents
log_dir=$home/Library/Logs/RemoteMAC

case "$(uname -m)" in
  arm64|aarch64)
    arch=arm64
    asset=remotemac-darwin-arm64.zip
    binary_entry=remotemac-darwin-arm64
    ;;
  x86_64|amd64)
    arch=amd64
    asset=remotemac-darwin-amd64.zip
    binary_entry=remotemac-darwin-amd64
    ;;
  *) die "不支持的 Mac 架构：$(uname -m)" ;;
esac

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}
require_command launchctl
require_command grep
require_command sed
[ "$uid" -eq 0 ] || require_command sudo

tmp= tty_state=
restore_tty() {
  if [ -n "$tty_state" ]; then
    stty "$tty_state" < /dev/tty 2>/dev/null || :
    tty_state=
  fi
}
cleanup() {
  restore_tty
  if [ -n "$tmp" ]; then
    rm -f "$tmp/archive.zip" "$tmp/binary" "$tmp/SHA256SUMS" \
      "$tmp/password" "$tmp/settings" "$tmp/run" "$tmp/unit.plist" \
      "$tmp/installed"
    rmdir "$tmp" 2>/dev/null || :
  fi
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

[ -r /dev/tty ] && [ -w /dev/tty ] || die '需要交互终端才能完成安装'
ask() {
  printf '%s' "$1" > /dev/tty
  answer=
  IFS= read -r answer < /dev/tty || die '输入已取消'
}
confirm() {
  ask "$1 [y/N]："
  case "$answer" in y|Y) ;; *) die '已取消' ;; esac
}
priv() {
  if [ "$(id -u)" -eq 0 ]; then
    [ "${1:-}" != -v ] || return 0
    "$@"
  else
    sudo "$@"
  fi
}

managed=false
for managed_path in "$base" "$config" "$unit" "$launch_agents" "$log_dir"; do
  [ ! -L "$managed_path" ] || die "受管理路径不能是符号链接：$managed_path"
done
if [ -f "$base/installed" ] && grep -qx 'RemoteMAC' "$base/installed"; then managed=true; fi

if [ "$action" = uninstall ]; then
  [ "$managed" = true ] || die '没有找到 RemoteMAC 安装记录'
  confirm '卸载 RemoteMAC？将停止并禁用登录启动服务，并删除 RemoteMAC 管理的程序、口令、配置、启动项、安装标记和日志'
  priv -v
  if launchctl print "gui/$uid/$label" >/dev/null 2>&1; then launchctl bootout "gui/$uid/$label"; fi
  launchctl disable "gui/$uid/$label" >/dev/null 2>&1 || :
  rm -f "$unit"
  if [ -e "$binary" ] || [ -L "$binary" ]; then priv rm -f "$binary"; fi
  rm -f "$config/password" "$config/settings"
  if [ -d "$config" ]; then rmdir "$config" 2>/dev/null || say "配置目录含其他文件，予以保留：$config"; fi
  rm -f "$base/run" "$base/installed"
  if [ -d "$base" ]; then rmdir "$base" 2>/dev/null || say "数据目录含其他文件，予以保留：$base"; fi
  rm -f "$log_dir/server.log" "$log_dir/server-error.log"
  if [ -d "$log_dir" ]; then rmdir "$log_dir" 2>/dev/null || say "日志目录含其他文件，予以保留：$log_dir"; fi
  say 'RemoteMAC 已卸载：已停止并禁用登录启动服务，RemoteMAC 管理的程序、口令、配置、启动项、安装标记和日志已删除。含其他文件的目录已保留。'
  exit 0
fi

printf '%s\n' "$repo" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || die '请用 --repo 指定 owner/repository'
if [ -n "$version" ]; then
  printf '%s\n' "$version" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]*$' || die '版本 tag 含不支持的字符'
fi

require_command curl
require_command unzip
require_command awk
require_command shasum
require_command install
require_command plutil

if [ -e "$base" ] || [ -L "$base" ]; then
  [ "$managed" = true ] || die "安装目录已存在且不是本安装器创建的：$base"
fi
if [ -e "$config" ] || [ -L "$config" ]; then
  [ "$managed" = true ] || die "配置目录已存在且不是本安装器创建的：$config"
fi
if [ -e "$binary" ] || [ -L "$binary" ]; then
  [ ! -L "$binary" ] || die "程序路径不能是符号链接：$binary"
  [ "$managed" = true ] || die "程序路径已存在，为避免覆盖其他文件而停止：$binary"
fi
if [ -L "$unit" ]; then die "启动项不能是符号链接：$unit"; fi
if [ -e "$unit" ] || [ -L "$unit" ]; then
  [ "$managed" = true ] || die "启动项已存在且不属于 RemoteMAC：$unit"
fi
[ ! -L /usr/local/bin ] || die '/usr/local/bin 不能是符号链接'
[ ! -e /usr/local/bin ] || [ -d /usr/local/bin ] || die '/usr/local/bin 不是目录'

tmp=$(mktemp -d "${TMPDIR:-/tmp}/remotemac-install.XXXXXXXX")
fetch() {
  curl --fail --location --silent --show-error --proto '=https' --proto-redir '=https' \
    --connect-timeout 15 --max-time 300 "$1" -o "$2"
}
if [ -n "$version" ]; then
  release_url=https://github.com/$repo/releases/download/$version
  release_label=$version
else
  release_url=https://github.com/$repo/releases/latest/download
  release_label=latest
fi
say "下载 $repo $release_label Release：$asset 与 SHA256SUMS…"
fetch "$release_url/$asset" "$tmp/archive.zip"
fetch "$release_url/SHA256SUMS" "$tmp/SHA256SUMS"

expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$tmp/SHA256SUMS")
[ "${#expected}" = 64 ] || die '校验和清单缺少该资产或包含重复项'
case "$expected" in *[!0-9a-fA-F]*) die '校验和格式错误';; esac
actual=$(shasum -a 256 "$tmp/archive.zip" | awk '{print $1}')
expected_lower=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
[ "$expected_lower" = "$actual" ] || die 'SHA-256 校验失败'
unzip -p "$tmp/archive.zip" "$binary_entry" > "$tmp/binary" || die 'ZIP 中缺少目标二进制或文件损坏'
[ -s "$tmp/binary" ] || die 'ZIP 中的目标二进制为空'
chmod 755 "$tmp/binary"

ask '监听地址 [0.0.0.0]（IPv4 或 IPv6，不加方括号）：'
address=${answer:-0.0.0.0}
case "$address" in
  *:*) case "$address" in *[!A-Fa-f0-9:.%_-]*) die 'IPv6 地址格式无效';; esac ;;
  *)
    case "$address" in *[!0-9.]*|'') die '请输入 IPv4 地址';; esac
    printf '%s\n' "$address" | awk -F. 'NF != 4 { exit 1 } { for (i=1;i<=4;i++) if ($i !~ /^[0-9]+$/ || $i > 255) exit 1; exit 0 }' || die 'IPv4 地址格式无效'
    ;;
esac
ask '监听端口 [28080]：'
port=${answer:-28080}
case "$port" in ''|*[!0-9]*) die '端口必须是 1–65535 的整数';; esac
[ "${#port}" -le 5 ] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || die '端口范围为 1–65535'
case "$address" in *:*) listen="[$address]:$port";; *) listen="$address:$port";; esac

tty_state=$(stty -g < /dev/tty)
stty -echo < /dev/tty
ask '设置 RemoteMAC 访问口令：'
token=$answer
printf '\n' > /dev/tty
ask '再次输入访问口令：'
token_repeat=$answer
printf '\n' > /dev/tty
restore_tty
[ -n "$token" ] && [ "$token" = "$token_repeat" ] || die '口令为空或两次输入不一致'
unset token_repeat answer

say "用户：${user}；来源：${repo}；版本：${release_label}；架构：${arch}；监听：${listen}"
say 'macOS 将在当前用户登录后启动 RemoteMAC；服务不会在无人登录的情况下运行。'
case "$address" in
  127.*|::1|::1%*) ;;
  *) say '警告：当前监听地址允许远程连接。HTTP 登录和信令未加密，请仅用于可信局域网或受信任的 VPN。' ;;
esac
if [ "$managed" = true ]; then
  confirm '检测到现有 RemoteMAC 安装；确认替换为最新 Release 版本？'
else
  confirm '确认安装并在登录后自动启动 RemoteMAC？'
fi

# Quote values as literal POSIX shell strings before the runner sources settings.
quote() {
  printf "'"
  printf '%s' "$1" | sed "s/'/'\\\\''/g"
  printf "'"
}
printf '%s' "$token" > "$tmp/password"
unset token
{
  printf 'listen='; quote "$listen"; printf '\n'
  printf 'binary='; quote "$binary"; printf '\n'
} > "$tmp/settings"
cat > "$tmp/run" <<'RUNNER'
#!/bin/sh
set -eu
config=$1
. "$config/settings"
REMOTEMAC_TOKEN=$(cat "$config/password")
export REMOTEMAC_TOKEN
exec "$binary" -host "$listen"
RUNNER
chmod 700 "$tmp/run"

xml_escape() {
  printf '%s' "$1" | sed -e 's/\&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}
plist_base=$(xml_escape "$base")
plist_config=$(xml_escape "$config")
plist_log_dir=$(xml_escape "$log_dir")
cat > "$tmp/unit.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>/bin/sh</string><string>$plist_base/run</string><string>$plist_config</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>Umask</key><integer>63</integer>
  <key>StandardOutPath</key><string>$plist_log_dir/server.log</string>
  <key>StandardErrorPath</key><string>$plist_log_dir/server-error.log</string>
</dict></plist>
EOF
plutil -lint "$tmp/unit.plist" >/dev/null

priv -v
mkdir -p "$base" "$config" "$launch_agents" "$log_dir"
chmod 700 "$base" "$config" "$log_dir"
printf '%s\n' RemoteMAC > "$base/installed"
if launchctl print "gui/$uid/$label" >/dev/null 2>&1; then
  launchctl bootout "gui/$uid/$label"
fi
priv install -d -m 755 /usr/local/bin
priv install -m 755 "$tmp/binary" "$binary"
install -m 600 "$tmp/password" "$config/password"
install -m 600 "$tmp/settings" "$config/settings"
install -m 700 "$tmp/run" "$base/run"
install -m 600 "$tmp/unit.plist" "$unit"
launchctl enable "gui/$uid/$label"
launchctl bootstrap "gui/$uid" "$unit"

case "$address" in
  0.0.0.0|127.*) probe_host=127.0.0.1 ;;
  ::) probe_host='[::1]' ;;
  *:*) probe_host="[$(printf '%s' "$address" | sed 's/%/%25/g')]" ;;
  *) probe_host=$address ;;
esac
probe_url=http://$probe_host:$port
ready=false
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
  running=false
  if launchctl print "gui/$uid/$label" 2>/dev/null | grep -q 'state = running'; then running=true; fi
  health_code=$(curl --noproxy '*' --silent --output /dev/null --write-out '%{http_code}' --max-time 2 "$probe_url/healthz" || :)
  api_code=$(curl --noproxy '*' --silent --output /dev/null --write-out '%{http_code}' --max-time 2 "$probe_url/api/capabilities" || :)
  if [ "$running" = true ] && [ "$health_code" = 200 ] && [ "$api_code" = 401 ]; then ready=true; break; fi
  sleep 1
done
[ "$ready" = true ] || die "服务未通过健康与鉴权检查；配置保留，可查看日志：$log_dir"

say "RemoteMAC 已安装并启动：$probe_url"
say "来源：${repo}；版本：${release_label}；资产：${asset}"
say "程序：$binary"
say "登录启动项：$unit"
say "口令文件（权限 600）：$config/password"
say "日志：$log_dir"
say '请从控制端连接一次 RemoteMAC，并按 macOS 提示授权屏幕录制和设备控制（辅助功能）。'
say '完成两项授权后，请重启服务：'
say "launchctl kickstart -k \"gui/$(id -u)/${label}\""
