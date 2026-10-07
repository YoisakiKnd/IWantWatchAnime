# 装到玩客云（armv7）

这份文档只讲一件事：把 `bin/suzu-linux-armv7` 装到那台 eMMC 不能覆写、移动硬盘不能引导的盒子上，
并且**整个过程不写根文件系统**。

## 0. 为什么这么装

| 你踩过的坑 | 这份部署怎么绕开 |
|---|---|
| eMMC 无法覆写 | suzu 的二进制、配置、数据库、下载、媒体库**全部在外接盘**；systemd 单元里 `ProtectSystem=strict`，除了显式授权的三个目录，整个文件系统对它只读。它没有能力写坏 eMMC。 |
| 移动硬盘无法引导 | suzu 完全不碰引导。你的系统怎么起来还是怎么起来（eMMC 里的原系统 / U 盘 / SD 卡都行），suzu 只是系统起来之后挂在盘上的一个普通服务。 |
| 没有 armv7 的看番方案 | 静态链接的 `ELF 32-bit ARM` 二进制，15 MB，无 CGO 无动态库依赖。`aria2` 用系统包管理器装。 |

## 1. 拿到二进制（两条路，选一条）

**路 A：直接从 release 下（盒子上不需要 Go）**

每个 tag 都会自动出包，下 armv7 那个部署包，解开就齐了：

```bash
cd /tmp
curl -fLO https://github.com/YoisakiKnd/IWantWatchAnime/releases/latest/download/suzu-linux-armv7
curl -fLO https://github.com/YoisakiKnd/IWantWatchAnime/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS        # 先验校验和，再谈安装
chmod +x suzu-linux-armv7
```

想要"连部署材料一起"的那种包：下 `suzu-<版本>-armv7.tar.gz`，解开是
`suzu-linux-armv7` + `deploy/install.sh` + 配置模板 + systemd 单元 + 这份文档。

**路 B：自己编（想改代码时走这条）**

```bash
make armv7                 # → bin/suzu-linux-armv7
file bin/suzu-linux-armv7   # 应为 ELF 32-bit LSB executable, ARM, EABI5, statically linked
make dist                  # 想要打包好的那种：四个平台 + SHA256SUMS + armv7 部署包
```
把这个二进制、`deploy/` 整个目录一起传到盒子上（`scp -r` 或 U 盘拷）。

## 2. 先把盘挂稳（这一步最容易被忽略）

盘挂载点会漂（`/dev/sda1` 可能变 `/dev/sdb1`），一漂 suzu 就会去写 eMMC 上的空目录。**按 UUID 挂**：

```bash
blkid                       # 找到那块盘的 UUID
mkdir -p /mnt/usb
echo 'UUID=<你的UUID> /mnt/usb ext4 defaults,noatime,nofail 0 2' >> /etc/fstab
mount -a && findmnt /mnt/usb && df -h /mnt/usb
```

`nofail` 很关键：盘不在的时候系统照样能起来，你能 SSH 进去修。

## 3. 装

```bash
sudo SUZU_DATA=/mnt/usb deploy/install.sh
```

脚本会做：建目录（`suzu/`、`downloads/`、`library/`）→ 拷二进制 → 用 `deploy/config.armv7.toml`
渲染一份 `config.toml`（随机生成面板密码并打印）→ 建 `suzu` 系统用户 → 装 `suzu.service` 并 `enable --now`
→ 等 `/healthz` 起来。

- 盘挂在别处：`SUZU_DATA=/mnt/sda1 sudo -E deploy/install.sh`
- 想先只摆文件、不动 systemd：`SUZU_NO_SYSTEMD=1 SUZU_PREFIX=/tmp/try deploy/install.sh`
- **脚本会拒绝在"不是挂载点的目录"上装**，除非你显式跳过 —— 防的就是"盘没挂上，数据全写进 eMMC"。

## 4. 装 aria2

```bash
sudo apt-get install -y aria2
sudo tee /etc/systemd/system/aria2.service >/dev/null <<'EOF'
[Unit]
Description=aria2 RPC
After=network-online.target
[Service]
User=suzu
ExecStart=/usr/bin/aria2c --enable-rpc --rpc-listen-all=false --rpc-listen-port=6800 \
  --dir=/mnt/usb/downloads --max-concurrent-downloads=2 --max-connection-per-server=4 \
  --max-peers=30 --seed-time=0 --continue=true --file-allocation=none --rpc-secret=
Restart=always
RestartSec=5
# A5 只有四核且单核弱：给下载器留点余地，别把面板和 SSH 挤掉
CPUWeight=80
IOSchedulingClass=best-effort
IOSchedulingPriority=4
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload && sudo systemctl enable --now aria2
```

对得上的三处：`--dir` 与配置里的 `engine.aria2.dir`、`--rpc-listen-port=6800` 与 `endpoint`、
`--seed-time=0`（下完就停，不给这块小盘做种）。`--file-allocation=none` 是给 ext4/小盘用的：
默认的 `falloc` 会先把整集空间占住，1GB 的盒子上下到一半盘满比下不动更难受。

## 5. 打开面板看结果

面板默认只监听 `127.0.0.1`（**这是故意的**：它旁边的局域网里可能坐着电视、手机、访客设备，
而这个面板能删订阅、能删文件）。用 SSH 端口转发看：

```bash
# 在笔记本上
ssh -L 8637:127.0.0.1:8637 root@玩客云
# 然后浏览器开 http://127.0.0.1:8637，账号 admin + 安装时打印的密码
```

想在手机/电视上直接看面板：把 `config.toml` 的 `listen` 改成 `0.0.0.0:8637`，
**同时**把 `user`/`password` 设成非空，否则程序会直接拒绝启动（这是故意的，不是 bug）。

## 6. 装完之后核对这五条

```bash
systemctl status suzu --no-pager            # active (running)
journalctl -u suzu -n 30 --no-pager         # 目标=linux/arm、下载内核就绪、面板已启动
curl -s localhost:8637/healthz              # ok dev uptime=…
findmnt /mnt/usb                            # 盘真挂着
ls -la /mnt/usb/suzu /mnt/usb/downloads     # 数据在外接盘上
```

然后：面板 →「番剧库」→ 搜一部番 → 勾一个字幕组 → 一键订阅。第一轮轮询会把**最新一集**投出去
（默认只跟新的；想连往期一起下，订阅时勾"连往期一起下"）。

## 7. 空间与硬链接

`link_mode = "hardlink"` 让做种与入库共用同一份数据。**前提是下载目录与媒体库在同一个文件系统上**：
按上面的部署，两个都在 `/mnt/usb` 下，满足。如果你把媒体库指到另一块盘，
硬链接会失败并自动退化成复制 —— 那意味着同一集在盘上存在两份，1TB 的盘很快会见底，日志里会有提示。

## 8. 出问题先看哪里

| 现象 | 先看 |
|---|---|
| 服务起不来 | `journalctl -u suzu -n 50`；大概率是配置里 `data_dir` 指向的盘没挂上 |
| 面板打不开 | `config.toml` 的 `listen` 是不是 `0.0.0.0`，以及是不是设了密码（没密码会被拒绝启动） |
| 投递失败、503 | `systemctl status aria2`；`curl localhost:6800/jsonrpc -d '{"jsonrpc":"2.0","id":"1","method":"aria2.getVersion"}'` |
| 下完没入库 | `link_mode` 与两个目录是否同一个文件系统；`df -h /mnt/usb` 是否还有空间 |
| 内存涨得凶 | `systemctl status suzu` 的 Memory 一栏；单元里 `MemoryMax=192M` 是护栏 |
| 根文件系统被写 | 不该发生。`systemd-analyze security suzu` 能看沙箱评分；写只允许在那三个目录 |

## 9. 已知没验过的部分

- 这套部署**没有在玩客云本体上跑过**：armv7 二进制已在 x86 上用 qemu-arm 跑通全链路（面板、轮询、
  投递、硬链接入库、通知），但设备上的真实内存/CPU 与 USB 盘行为要以你那边为准（见 `ARCHITECTURE.md` §9）。
- USB 上的文件系统若是 `exFAT`/`NTFS`，**硬链接不可用**（会退化成复制），权限模型也不一样。
  媒体库想省空间就用 `ext4`；只想让盒子读 U 盘里的片源，那 `exFAT` 更省事。