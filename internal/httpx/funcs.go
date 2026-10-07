package httpx

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// funcMap 只放模板真正需要的格式化函数，保持视图层零业务逻辑。
var funcMap = template.FuncMap{
	"clock":      clock,
	"day":        day,
	"bytes":      humanBytes,
	"dur":        humanDur,
	"since":      func(t time.Time) string { return humanDur(time.Since(t)) },
	"taskLabel":  taskLabel,
	"itemLabel":  itemLabel,
	"pct":        func(f float64) string { return strconv.Itoa(int(f + 0.5)) },
	"ep":         epLabel,
	"epLabel":    epLabel,
	"libEpLabel": libEpLabel,
	"kindLabel":  kindLabel,
	// barStyle 返回整条 CSS 值并标记为安全：进度是受控的 0..100 浮点数，
	// 交给 html/template 的 CSS 过滤器反而会被转成 ZgotmplZ。
	"barStyle": func(f float64) template.CSS {
		if f < 0 {
			f = 0
		}
		if f > 100 {
			f = 100
		}
		return template.CSS("width:" + strconv.FormatFloat(f, 'f', 1, 64) + "%")
	},
	"untild": func(t, now time.Time) string {
		if t.IsZero() {
			return ""
		}
		return humanDur(t.Sub(now))
	},
	"statusCountLabel": func(s string) string { return itemLabel(model.ItemStatus(s)) },
	"fansub":           fansubOf,
	"initial":          initialOf,
	"shortTitle":       shortTitle,
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "--:--"
	}
	return t.Format("15:04")
}

func day(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return fmt.Sprintf("%d月%d日 %s", int(t.Month()), t.Day(), t.Format("15:04"))
}

// humanDur 输出「2 时 18 分后」这类相对时间；过去的时间加「前」。
func humanDur(d time.Duration) string {
	neg := d < 0
	if neg {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		s = fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		s = fmt.Sprintf("%d 分", int(d.Minutes()))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%d 时 %d 分", int(d.Hours()), int(d.Minutes())%60)
	default:
		s = fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
	if neg {
		return s + "前"
	}
	return s + "后"
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func taskLabel(s model.TaskState) string {
	switch s {
	case model.TaskQueued:
		return "排队中"
	case model.TaskDownloading:
		return "下载中"
	case model.TaskPaused:
		return "已暂停"
	case model.TaskSeeding:
		return "做种中"
	case model.TaskCompleted:
		return "已完成"
	case model.TaskError:
		return "失败"
	}
	return string(s)
}

func itemLabel(s model.ItemStatus) string {
	switch s {
	case model.ItemNew:
		return "待判定"
	case model.ItemMatched:
		return "已投递"
	case model.ItemRejected:
		return "已过滤"
	case model.ItemDone:
		return "已入库"
	case model.ItemFailed:
		return "失败"
	}
	return string(s)
}

// epLabel 把集数渲染成「第 3 集」/「合集 1-12」/「剧场版」。
func epLabel(it model.Item) string {
	switch {
	case it.Batch && it.EpisodeTo > it.Episode:
		return fmt.Sprintf("合集 %s-%s", trimNum(it.Episode), trimNum(it.EpisodeTo))
	case it.Episode > 0:
		return "第 " + trimNum(it.Episode) + " 集"
	default:
		return "整部"
	}
}

// libEpLabel 服务于入库列表：入库记录只保留了集数，没有合集区间。
func libEpLabel(e model.LibraryEntry) string {
	if e.Episode > 0 {
		return "第 " + trimNum(e.Episode) + " 集"
	}
	return "整部"
}

func kindLabel(it model.Item) string {
	if it.Batch {
		return "合集"
	}
	if it.Episode == 0 {
		return "整部"
	}
	return "单集"
}

func trimNum(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s
}

// shortTitle 只截断超长标题，不改变语义；用于窄栏。
// fansubOf 从订阅名「葬送的芙莉莲 [喵萌奶茶屋]」里取出字幕组名。
func fansubOf(name string) string {
	if i := strings.LastIndex(name, "["); i >= 0 && strings.HasSuffix(name, "]") {
		return name[i+1 : len(name)-1]
	}
	return name
}

// initialOf 取标题首字，作为封面加载失败时的占位字。
// 蜜柑偶尔会换图床，一个汉字占位比一张裂图体面得多。
func initialOf(s string) string {
	for _, r := range s {
		if r == ' ' || r == '　' {
			continue
		}
		return string(r)
	}
	return "番"
}

func shortTitle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
