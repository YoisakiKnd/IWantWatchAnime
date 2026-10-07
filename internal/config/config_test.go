package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 默认值必须「开箱可用」：换链默认开着，阈值是这个项目里最要紧的数字之一。
func TestDefaultRelink(t *testing.T) {
	c := Default()
	if !c.Relink.Enabled {
		t.Error("断种换链应当默认开启（老番的种子本来就常常没人做种）")
	}
	if c.Relink.StuckHours != 6 {
		t.Errorf("默认卡住阈值应当 6 小时，实际 %d", c.Relink.StuckHours)
	}
	if c.Relink.MaxPerEp != 2 {
		t.Errorf("默认每集最多换 2 次，实际 %d", c.Relink.MaxPerEp)
	}
}

// 写错的阈值不能直接生效：小于 1 小时会把「刚投递还没连上 peer」误判成断种，
// 大于 3 天又等于没有这个功能。
func TestValidateClampsRelink(t *testing.T) {
	cases := []struct {
		stuck, max, wantStuck, wantMax int
	}{
		{0, 0, 1, 1},
		{-5, -1, 1, 1},
		{1, 1, 1, 1},
		{6, 2, 6, 2},
		{999, 99, 72, 5},
	}
	for _, c := range cases {
		c := c
		cfg := Default()
		cfg.Relink.StuckHours = c.stuck
		cfg.Relink.MaxPerEp = c.max
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if cfg.Relink.StuckHours != c.wantStuck || cfg.Relink.MaxPerEp != c.wantMax {
			t.Errorf("StuckHours=%d MaxPerEp=%d → 期望 %d/%d，实际 %d/%d",
				c.stuck, c.max, c.wantStuck, c.wantMax, cfg.Relink.StuckHours, cfg.Relink.MaxPerEp)
		}
	}
}

// [relink] 这一节要能从 TOML 里读进来（字段名写错就静默用默认值，很坑）。
func TestLoadRelinkBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[relink]
enabled = false
stuck_hours = 12
max_per_episode = 3
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Relink.Enabled {
		t.Error("enabled = false 没读进来")
	}
	if c.Relink.StuckHours != 12 {
		t.Errorf("stuck_hours 没读进来：%d", c.Relink.StuckHours)
	}
	if c.Relink.MaxPerEp != 3 {
		t.Errorf("max_per_episode 没读进来：%d", c.Relink.MaxPerEp)
	}
	// 同一份文件里没写的字段要保留默认值。
	if c.Library.LinkMode != Default().Library.LinkMode {
		t.Errorf("没写的字段应当保留默认值，实际 %q", c.Library.LinkMode)
	}
}

// bgm_base 让简介/评分可以走镜像或自建 Bangumi；不写就是官方地址。
// 只写域名时补上 https://，省得用户因为少打一个协议头而静默失效。
func TestLoadBGMBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[metadata]
enabled = true
provider = "mikan"
bgm_base = "bgm.example.com/"
bgm_token = "tok-123"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil { // main 启动时就是这么调用的
		t.Fatal(err)
	}
	if c.Meta.BGMBase != "https://bgm.example.com/" {
		t.Errorf("bgm_base 没按预期归一化：%q", c.Meta.BGMBase)
	}
	if c.Meta.BGMToken != "tok-123" {
		t.Errorf("bgm_token 没读进来：%q", c.Meta.BGMToken)
	}

	// 不写 bgm_base 时留空，由客户端回退到官方 api.bgm.tv。
	path2 := filepath.Join(dir, "c2.toml")
	if err := os.WriteFile(path2, []byte("[metadata]\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Meta.BGMBase != "" {
		t.Errorf("没配时应当留空（回退官方地址）：%q", c2.Meta.BGMBase)
	}
}

// 默认必须只监听本机：这个面板能删订阅、能删文件，出厂就在局域网上摊着不合适。
func TestDefaultListensOnLoopbackOnly(t *testing.T) {
	c := Default()
	if c.Server.Listen != "127.0.0.1:8637" {
		t.Fatalf("默认监听地址应为 127.0.0.1:8637，实际 %q", c.Server.Listen)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("默认配置应当能过校验: %v", err)
	}
}

// 没设账号密码就监听非本机地址 = 把操作按钮公开给局域网，必须拦下。
func TestValidateRefusesOpenPanel(t *testing.T) {
	open := []string{"0.0.0.0:8637", ":8637", "192.168.1.10:8637", "0.0.0.0:0"}
	for _, listen := range open {
		c := Default()
		c.Server.Listen = listen
		c.Server.User, c.Server.Password = "", ""
		if err := c.Validate(); err == nil {
			t.Errorf("listen=%q 且无密码时应当拒绝启动", listen)
		}
	}

	// 设了密码就允许对局域网开放（用户明确要的结果）。
	c := Default()
	c.Server.Listen = "0.0.0.0:8637"
	c.Server.User, c.Server.Password = "admin", "换一个不叫 admin 的密码"
	if err := c.Validate(); err != nil {
		t.Errorf("有密码时应当允许监听局域网: %v", err)
	}

	// 只监听本机时，没密码也允许（没法从别的机器访问到）。
	for _, listen := range []string{"127.0.0.1:8637", "localhost:8637", "[::1]:8637"} {
		c := Default()
		c.Server.Listen = listen
		c.Server.User, c.Server.Password = "", ""
		if err := c.Validate(); err != nil {
			t.Errorf("只监听本机时不该拦: %v", err)
		}
	}
}
