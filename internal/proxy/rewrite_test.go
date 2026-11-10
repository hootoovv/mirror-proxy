package proxy

import (
	"strings"
	"testing"
)

func mkUp(t *testing.T, baseURL string) upstreamInfo {
	t.Helper()
	return upstreamInfo{name: baseURL, base: mustParseURL(baseURL), baseStr: strings.TrimRight(baseURL, "/")}
}

func TestRewriteHTML(t *testing.T) {
	ups := []upstreamInfo{
		mkUp(t, "https://mirrors.aliyun.com/pypi"),
		mkUp(t, "https://pypi.tuna.tsinghua.edu.cn"),
	}
	html := `<html><body>
<a href="https://mirrors.aliyun.com/pypi/packages/aa/foo-1.0.whl#sha256=abc">foo</a>
<a href="/pypi/packages/bb/bar-2.0.whl#sha256=def">bar</a>
<a href="/packages/cc/baz-3.0.whl#sha256=123">baz</a>
<a href="https://pypi.tuna.tsinghua.edu.cn/packages/dd/qux-4.0.whl">qux</a>
<a href="#sha256=keep">anchor</a>
<a href="subdir/relative.whl">rel</a>
</body></html>`
	out := string(RewriteHTML([]byte(html), "/pip", ups))
	for _, want := range []string{
		`href="/pip/packages/aa/foo-1.0.whl#sha256=abc"`,
		`href="/pip/packages/bb/bar-2.0.whl#sha256=def"`,
		`href="/pip/packages/cc/baz-3.0.whl#sha256=123"`,
		`href="/pip/packages/dd/qux-4.0.whl"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少重写结果 %s\n输出:\n%s", want, out)
		}
	}
	for _, keep := range []string{
		`href="#sha256=keep"`,
		`href="subdir/relative.whl"`,
	} {
		if !strings.Contains(out, keep) {
			t.Fatalf("不应改写 %s\n输出:\n%s", keep, out)
		}
	}
}

func TestRewriteHTMLRootPrefixNotDouble(t *testing.T) {
	// 带更长前缀的上游必须优先于根路径上游，避免产生 /pip/pypi/... 双前缀
	ups := []upstreamInfo{
		mkUp(t, "https://pypi.tuna.tsinghua.edu.cn"),
		mkUp(t, "https://mirrors.aliyun.com/pypi"),
	}
	out := string(RewriteHTML([]byte(`<a href="/pypi/packages/x.whl">`), "/pip", ups))
	if !strings.Contains(out, `href="/pip/packages/x.whl"`) {
		t.Fatalf("根相对链接重写错误: %s", out)
	}
	if strings.Contains(out, "/pip/pypi/") {
		t.Fatalf("出现双重前缀: %s", out)
	}
}

func TestRewriteJSON(t *testing.T) {
	ups := []upstreamInfo{
		mkUp(t, "https://registry.npmmirror.com"),
		mkUp(t, "https://registry.npmjs.org"),
	}
	js := `{"name":"react","dist":{"tarball":"https://registry.npmmirror.com/react/-/react-18.2.0.tgz","integrity":"sha512-x"}}`
	out := string(RewriteJSON([]byte(js), "/npm", "http://mirror.local:8080", ups))
	want := `"tarball": "http://mirror.local:8080/npm/react/-/react-18.2.0.tgz"`
	if !strings.Contains(out, want) {
		t.Fatalf("JSON 重写结果错误:\n%s", out)
	}
	// 未命中上游的 URL 保持不变
	js2 := `{"doc":"https://example.com/x","dist":{"tarball":"https://example.com/y.tgz"}}`
	out2 := string(RewriteJSON([]byte(js2), "/npm", "http://mirror.local:8080", ups))
	if out2 != js2 {
		t.Fatalf("无关 URL 不应被改写: %s", out2)
	}
}

func TestRewriteAttrURLCases(t *testing.T) {
	ms := buildMatchers([]upstreamInfo{
		mkUp(t, "https://mirrors.aliyun.com/pypi"),
		mkUp(t, "https://pypi.tuna.tsinghua.edu.cn"),
	})
	cases := []struct {
		in, want string
	}{
		{"https://mirrors.aliyun.com/pypi/packages/a.whl", "/pip/packages/a.whl"},
		{"http://mirrors.aliyun.com/pypi/simple/", "/pip/simple/"},
		{"https://pypi.tuna.tsinghua.edu.cn/packages/b.whl", "/pip/packages/b.whl"},
		{"/pypi/packages/c.whl", "/pip/packages/c.whl"},
		{"/packages/d.whl", "/pip/packages/d.whl"},
		{"/favicon.ico", ""}, // 根路径上游（tuna）存在时，所有根相对链接都会映射
		{"mailto:a@b.c", "mailto:a@b.c"},
		{"relative/file.whl", "relative/file.whl"},
		{"#frag", "#frag"},
		{"https://other.host/x", "https://other.host/x"},
	}
	for _, tc := range cases {
		got := rewriteAttrURL(tc.in, ms, "/pip", "")
		if tc.want == "" {
			if got != "/pip/favicon.ico" {
				t.Fatalf("rewriteAttrURL(%q) = %q, 期望 /pip/favicon.ico", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("rewriteAttrURL(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
	// 绝对模式（JSON）
	got := rewriteAttrURL("https://mirrors.aliyun.com/pypi/packages/a.whl", ms, "/pip", "http://p:1")
	if got != "http://p:1/pip/packages/a.whl" {
		t.Fatalf("绝对模式重写错误: %q", got)
	}
}
