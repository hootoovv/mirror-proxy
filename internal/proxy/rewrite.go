package proxy

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// URLMatcher 描述一个上游的 URL 形态，用于把上游返回内容中的绝对链接
// 映射回代理路由（pip 索引页、npm 元数据 JSON 等）。
type URLMatcher struct {
	host     string // host[:port]
	basePath string // 形如 "/pypi"；站点根路径的上游为 ""
}

func newURLMatcher(u upstreamInfo) URLMatcher {
	return URLMatcher{
		host:     u.base.Host,
		basePath: strings.TrimRight(u.base.Path, "/"),
	}
}

// buildMatchers 按 basePath 从长到短排序：更长（更具体）的前缀优先匹配，
// 确保根路径上游（basePath 为空）不会抢先吞掉带前缀的链接。
func buildMatchers(ups []upstreamInfo) []URLMatcher {
	ms := make([]URLMatcher, 0, len(ups))
	for _, u := range ups {
		ms = append(ms, newURLMatcher(u))
	}
	sort.SliceStable(ms, func(i, j int) bool {
		return len(ms[i].basePath) > len(ms[j].basePath)
	})
	return ms
}

// rewriteAttrURL 重写单个 URL：
//   - 绝对 URL（含 scheme）或根相对 URL（以 / 开头）：若命中某上游，
//     映射为 route/<去掉上游前缀后的路径>；
//   - 其余（相对路径、锚点等）：原样返回（相对路径基于代理 URL 解析，
//     天然指向代理自身）。
//
// absBase 非空时返回绝对 URL（JSON 场景必须），否则返回根相对 URL（HTML 场景）。
func rewriteAttrURL(u string, ms []URLMatcher, route, absBase string) string {
	if u == "" || u[0] == '#' {
		return u
	}
	if i := strings.Index(u, "://"); i > 0 {
		rest, ok := matchAbsolute(u[i+3:], ms)
		if !ok {
			return u
		}
		if absBase != "" {
			return absBase + route + "/" + rest
		}
		return route + "/" + rest
	}
	if u[0] == '/' {
		for _, m := range ms {
			if m.basePath == "" {
				if absBase != "" {
					return absBase + route + u
				}
				return route + u
			}
			if strings.HasPrefix(u, m.basePath+"/") {
				rest := u[len(m.basePath):] // 保留开头 "/" → route + rest
				if absBase != "" {
					return absBase + route + "/" + strings.TrimPrefix(rest, "/")
				}
				return route + rest
			}
		}
	}
	return u
}

// matchAbsolute 在 hostPart（形如 host[:port]/path…）中匹配上游。
func matchAbsolute(hostPart string, ms []URLMatcher) (string, bool) {
	for _, m := range ms {
		if hostPart != m.host && !strings.HasPrefix(hostPart, m.host+"/") {
			continue
		}
		after := hostPart[len(m.host):]
		if after != "" && after[0] != '/' {
			continue
		}
		rest := strings.TrimPrefix(after, "/")
		if prefix := strings.TrimPrefix(m.basePath, "/"); prefix != "" {
			if !strings.HasPrefix(rest, prefix+"/") && rest != prefix {
				continue
			}
			rest = strings.TrimPrefix(rest, prefix+"/")
		}
		return rest, true
	}
	return "", false
}

var (
	htmlAttrDQ = regexp.MustCompile(`(?i)\b(href|src)\s*=\s*"([^"]*)"`)
	htmlAttrSQ = regexp.MustCompile(`(?i)\b(href|src)\s*=\s*'([^']*)'`)
	jsonField  = regexp.MustCompile(`"(tarball|resolved)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

// RewriteHTML 重写 HTML 中的 href/src 属性：
// 指向上游的绝对链接与根相对链接 → /<route>/…（根相对，客户端会基于代理
// 请求的 host 解析，天然无需感知代理对外地址）。
func RewriteHTML(data []byte, route string, ups []upstreamInfo) []byte {
	ms := buildMatchers(ups)
	s := string(data)
	s = htmlAttrDQ.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlAttrDQ.FindStringSubmatch(m)
		nu := rewriteAttrURL(sub[2], ms, route, "")
		if nu == sub[2] {
			return m
		}
		return sub[1] + `="` + nu + `"`
	})
	s = htmlAttrSQ.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlAttrSQ.FindStringSubmatch(m)
		nu := rewriteAttrURL(sub[2], ms, route, "")
		if nu == sub[2] {
			return m
		}
		return sub[1] + `='` + nu + `'`
	})
	return []byte(s)
}

// RewriteJSON 重写 JSON 中的 tarball/resolved 字段：
// 指向上游的链接 → <absBase>/<route>/…（必须是绝对 URL，JSON 无基准地址）。
func RewriteJSON(data []byte, route, absBase string, ups []upstreamInfo) []byte {
	ms := buildMatchers(ups)
	s := string(data)
	s = jsonField.ReplaceAllStringFunc(s, func(m string) string {
		sub := jsonField.FindStringSubmatch(m)
		nu := rewriteAttrURL(sub[2], ms, route, absBase)
		if nu == sub[2] {
			return m
		}
		return `"` + sub[1] + `": "` + nu + `"`
	})
	return []byte(s)
}

// 解析上游 URL（供测试构造 upstreamInfo 使用）。
func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
