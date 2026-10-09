package model

import (
	"testing"
	"time"
)

// 判定「已播完」用的三条都是真实抓下来的蜜柑数据。
// 它决定订阅一部完结番时会不会把往期一起下，判错了两边都难看：
// 判松了，订阅在播的番会把整部拉下来；判紧了，完结番一条都下不到。
func TestFinished(t *testing.T) {
	today := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		b    Bangumi
		now  time.Time // 零值表示用 today
		want bool
	}{
		{"向日葵马戏团：7/4 开播、13 集，9/26 播完", Bangumi{StartAt: "7/4/2026", Episodes: 13}, time.Time{}, true},
		{"芙莉莲第一季：9/29/2023 开播、28 集", Bangumi{StartAt: "9/29/2023", Episodes: 28}, time.Time{}, true},
		{"芙莉莲第二季：1/16/2026 开播、10 集", Bangumi{StartAt: "1/16/2026", Episodes: 10}, time.Time{}, true},

		{"还在播：上周才开播，共 12 集", Bangumi{StartAt: "10/2/2026", Episodes: 12}, time.Time{}, false},
		{"最后一集当天：还在宽限期内，先不补",
			Bangumi{StartAt: "7/4/2026", Episodes: 13},
			time.Date(2026, 9, 26, 23, 0, 0, 0, time.Local), false},
		{"过了宽限期就算播完",
			Bangumi{StartAt: "7/4/2026", Episodes: 13},
			time.Date(2026, 9, 28, 0, 30, 0, 0, time.Local), true},
		{"只有 1 集的番：开播当天即播完",
			Bangumi{StartAt: "7/4/2026", Episodes: 1},
			time.Date(2026, 7, 6, 0, 0, 0, 0, time.Local), true},

		{"缺总集数就不猜", Bangumi{StartAt: "7/4/2026"}, time.Time{}, false},
		{"缺放送开始就不猜", Bangumi{Episodes: 13}, time.Time{}, false},
		{"总集数是 0 就不猜", Bangumi{StartAt: "7/4/2026", Episodes: 0}, time.Time{}, false},
		{"日期看不懂就不猜", Bangumi{StartAt: "2026 年 7 月", Episodes: 13}, time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := c.now
			if now.IsZero() {
				now = today
			}
			if got := c.b.Finished(now); got != c.want {
				t.Errorf("Finished=%v，期望 %v", got, c.want)
			}
		})
	}
}

// 解析放送开始日期必须按「月/日/年」。
//
// 这条同时钉住了顺序：蜜柑的番剧页上写着「放送 星期六」，
// 而 2026-07-04 正好是星期六 —— 若按「日/月/年」解释会变成 4 月 7 日（星期二），
// 与页面对不上。前瞻取前两位的日/月顺序会静默算错一部番的完结时间。
func TestParseAirDateKeepsMonthFirst(t *testing.T) {
	got, ok := ParseAirDate("7/4/2026")
	if !ok {
		t.Fatal("7/4/2026 应当能解析")
	}
	if got.Month() != time.July || got.Day() != 4 || got.Year() != 2026 {
		t.Fatalf("解析成了 %s，期望 2026-07-04", got.Format("2006-01-02"))
	}
	if got.Weekday() != time.Saturday {
		t.Errorf("2026-07-04 应当是星期六（页面上写的「放送 星期六」），实际 %s", got.Weekday())
	}
	// 29 不可能是月份：这条日期只可能是 2023-09-29。
	if d, ok := ParseAirDate("9/29/2023"); !ok || d.Month() != time.September || d.Day() != 29 {
		t.Errorf("9/29/2023 解析错误：%v %v", d, ok)
	}
	if _, ok := ParseAirDate(""); ok {
		t.Error("空字符串不应当解析成功")
	}
}
