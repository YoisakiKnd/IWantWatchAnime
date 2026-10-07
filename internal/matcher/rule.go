package matcher

import (
	"regexp"
	"strings"
	"sync"
)

// Rule 是一条订阅上的过滤规则。
type Rule struct {
	Include []string // 命中任一即采纳；为空表示不限制
	Exclude []string // 命中任一即排除（优先于 Include）
	Prefer  []string // 字幕组/优先级标签，越靠前分越高
	StartEp float64  // 只收 >= 该集数的正片
}

// Decision 是判定结果。
type Decision struct {
	OK     bool
	Reason string
	Score  int
}

// 以 re: 开头的条件按正则处理，其余按不区分大小写的子串处理。
var reCache sync.Map // string -> *regexp.Regexp

func compile(pattern string) (*regexp.Regexp, bool) {
	if v, ok := reCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re, re != nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		reCache.Store(pattern, (*regexp.Regexp)(nil)) // 缓存失败，避免反复编译
		return nil, false
	}
	reCache.Store(pattern, re)
	return re, true
}

func hit(cond, s string) bool {
	if strings.HasPrefix(cond, "re:") {
		expr := strings.TrimSpace(cond[3:])
		re, ok := compile(expr)
		return ok && re.MatchString(s)
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(cond))
}

// Judge 判断一条解析结果是否采纳。
// 判定只依赖解析结果和标题本身：没有网络调用、没有数据库查询，
// 所以可以在轮询里对几百条条目无脑跑一遍。
func (r Rule) Judge(p Parsed, rawTitle string) Decision {
	for _, e := range r.Exclude {
		if e == "" {
			continue
		}
		if hit(e, rawTitle) {
			return Decision{OK: false, Reason: "命中排除条件：" + e}
		}
	}
	if len(r.Include) > 0 {
		matched := ""
		for _, in := range r.Include {
			if in == "" {
				continue
			}
			if hit(in, rawTitle) {
				matched = in
				break
			}
		}
		if matched == "" {
			return Decision{OK: false, Reason: "未命中包含条件"}
		}
	}
	if p.Kind == KindTV && r.StartEp > 0 && p.Episode > 0 && p.Episode < r.StartEp {
		return Decision{OK: false, Reason: "集数低于起始集"}
	}

	reason := "规则通过"
	score := 10
	if p.Batch || p.Kind == KindMovie {
		score = 5 // 合集/剧场版体积大，同集撞车时让位给单集
	}
	for i, pref := range r.Prefer {
		if pref == "" {
			continue
		}
		// 字幕组名或标题命中偏好标签；越靠前分越高。
		if hit(pref, p.Fansub) || hit(pref, rawTitle) {
			score += 100 - i*5
			reason = "偏好命中：" + pref
			break
		}
	}
	return Decision{OK: true, Reason: reason, Score: score}
}
