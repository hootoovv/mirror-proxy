package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"mirror-proxy/internal/cache"
	"mirror-proxy/internal/config"
)

// countingUpstream 一个可编程的假上游：按路径返回内容，并统计每个路径被请求的次数。
type countingUpstream struct {
	mu     sync.Mutex
	hits   map[string]int
	server *httptest.Server
}

func newCountingUpstream(handler http.HandlerFunc) *countingUpstream {
	cu := &countingUpstream{hits: map[string]int{}}
	cu.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cu.mu.Lock()
		cu.hits[r.URL.Path]++
		cu.mu.Unlock()
		handler(w, r)
	}))
	return cu
}

func (cu *countingUpstream) hit(path string) int {
	cu.mu.Lock()
	defer cu.mu.Unlock()
	return cu.hits[path]
}

func (cu *countingUpstream) total() int {
	cu.mu.Lock()
	defer cu.mu.Unlock()
	n := 0
	for _, v := range cu.hits {
		n += v
	}
	return n
}

func (cu *countingUpstream) close() { cu.server.Close() }

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func buildProxy(t *testing.T, cacheDir string, sources map[string]*config.SourceConfig) (*Proxy, *gin.Engine, *cache.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Sources: sources}
	cfg.Server.Addr = ":0"
	cfg.Server.CacheDir = cacheDir
	cfg.Server.MaxRewriteSize = 1 << 20
	cfg.Server.ResponseHeaderTimeout = config.Duration{Duration: 5 * time.Second}
	store, err := cache.New(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, store, newDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	p.Register(r)
	return p, r, store
}

func do(r *gin.Engine, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMissThenHit(t *testing.T) {
	up := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello-apt-cache")
	})
	defer up.close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{Name: "u1", BaseURL: up.server.URL}}},
	})

	// 第一次：MISS，回源
	w := do(r, http.MethodGet, "/apt/ubuntu/dists/jammy/InRelease", nil)
	if w.Code != 200 || w.Body.String() != "hello-apt-cache" {
		t.Fatalf("首次请求: code=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mirror-Cache"); got != "MISS" {
		t.Fatalf("X-Mirror-Cache = %q, 期望 MISS", got)
	}
	if up.hit("/ubuntu/dists/jammy/InRelease") != 1 {
		t.Fatal("上游应只被请求 1 次")
	}

	// 第二次：HIT，不再回源
	w = do(r, http.MethodGet, "/apt/ubuntu/dists/jammy/InRelease", nil)
	if w.Code != 200 || w.Body.String() != "hello-apt-cache" {
		t.Fatalf("二次请求: code=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Mirror-Cache"); got != "HIT" {
		t.Fatalf("X-Mirror-Cache = %q, 期望 HIT", got)
	}
	if up.total() != 1 {
		t.Fatalf("上游总请求数 = %d, 期望 1", up.total())
	}

	// 缓存文件位置符合布局：<dir>/apt/<In>/<请求目录>/<文件名>~
	want := filepath.Join(dir, "apt", "In", "ubuntu", "dists", "jammy", "InRelease~")
	bs, err := os.ReadFile(want)
	if err != nil || string(bs) != "hello-apt-cache" {
		t.Fatalf("缓存文件校验失败: %v", err)
	}

	// HEAD 请求命中缓存：无响应体
	w = do(r, http.MethodHead, "/apt/ubuntu/dists/jammy/InRelease", nil)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: code=%d bodyLen=%d", w.Code, w.Body.Len())
	}
	if up.total() != 1 {
		t.Fatal("HEAD 不应触发回源")
	}
}

// 用户补充的核心需求：apt 等源按 Ubuntu 版本分层缓存，
// 同名文件（不同版本的 Packages.gz）互不冲突。
func TestUbuntuVersionLayering(t *testing.T) {
	up := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		switch {
		case strings.Contains(r.URL.Path, "jammy"):
			fmt.Fprint(w, "PKGS-JAMMY")
		case strings.Contains(r.URL.Path, "noble"):
			fmt.Fprint(w, "PKGS-NOBLE")
		default:
			http.NotFound(w, r)
		}
	})
	defer up.close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{BaseURL: up.server.URL}}},
	})

	for _, v := range []string{"jammy", "noble"} {
		w := do(r, http.MethodGet, "/apt/ubuntu/dists/"+v+"/main/binary-amd64/Packages.gz", nil)
		if w.Code != 200 || w.Body.String() != "PKGS-"+strings.ToUpper(v) {
			t.Fatalf("%s 请求失败: code=%d body=%q", v, w.Code, w.Body.String())
		}
	}

	// 同名文件、不同版本 → 两个不同的缓存路径
	for _, v := range []string{"jammy", "noble"} {
		p := filepath.Join(dir, "apt", "Pa", "ubuntu", "dists", v, "main", "binary-amd64", "Packages.gz~")
		bs, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("缺少 %s 的缓存文件: %v", v, err)
		}
		if string(bs) != "PKGS-"+strings.ToUpper(v) {
			t.Fatalf("%s 缓存内容错误: %q", v, bs)
		}
	}
	// 各回源一次
	if up.hit("/ubuntu/dists/jammy/main/binary-amd64/Packages.gz") != 1 ||
		up.hit("/ubuntu/dists/noble/main/binary-amd64/Packages.gz") != 1 {
		t.Fatalf("上游请求计数错误: %+v", up.hits)
	}
	// 再次请求均命中且内容正确
	for _, v := range []string{"jammy", "noble"} {
		w := do(r, http.MethodGet, "/apt/ubuntu/dists/"+v+"/main/binary-amd64/Packages.gz", nil)
		if w.Header().Get("X-Mirror-Cache") != "HIT" || w.Body.String() != "PKGS-"+strings.ToUpper(v) {
			t.Fatalf("%s 二次请求错误: %q", v, w.Body.String())
		}
	}
}

func TestUpstreamFailoverInOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex

	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, "up1")
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up1.Close()

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, "up2")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "from-up2")
	}))
	defer up2.Close()

	up3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, "up3")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "from-up3")
	}))
	defer up3.Close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"npm": {Upstreams: []config.UpstreamConfig{
			{Name: "up1", BaseURL: up1.URL},
			{Name: "up2", BaseURL: up2.URL},
			{Name: "up3", BaseURL: up3.URL},
		}},
	})

	w := do(r, http.MethodGet, "/npm/left-pad/-/left-pad-1.0.0.tgz", nil)
	if w.Code != 200 || w.Body.String() != "from-up2" {
		t.Fatalf("应由 up2 提供服务: code=%d body=%q", w.Code, w.Body.String())
	}
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "up1,up2" {
		t.Fatalf("尝试顺序 = %q, 期望 up1,up2", got)
	}

	// 第二次请求命中缓存，不再触发任何上游
	w = do(r, http.MethodGet, "/npm/left-pad/-/left-pad-1.0.0.tgz", nil)
	if w.Header().Get("X-Mirror-Cache") != "HIT" {
		t.Fatal("第二次应命中缓存")
	}
	mu.Lock()
	got = strings.Join(order, ",")
	mu.Unlock()
	if got != "up1,up2" {
		t.Fatalf("命中后不应再回源: %q", got)
	}
}

func TestFailoverOn404(t *testing.T) {
	up1 := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	defer up1.close()
	up2 := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "only-on-up2")
	})
	defer up2.close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"npm": {Upstreams: []config.UpstreamConfig{
			{Name: "up1", BaseURL: up1.server.URL},
			{Name: "up2", BaseURL: up2.server.URL},
		}},
	})

	w := do(r, http.MethodGet, "/npm/rare-pkg", nil)
	if w.Code != 200 || w.Body.String() != "only-on-up2" {
		t.Fatalf("404 应故障转移到 up2: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestAllUpstreams404(t *testing.T) {
	up := newCountingUpstream(http.NotFound)
	defer up.close()
	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"pip": {Upstreams: []config.UpstreamConfig{{BaseURL: up.server.URL}}},
	})
	w := do(r, http.MethodGet, "/pip/packages/no-such-file.whl", nil)
	if w.Code != 404 {
		t.Fatalf("全部 404 时应返回 404, 实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "上游") {
		t.Fatalf("错误信息应说明上游情况: %s", w.Body.String())
	}
}

func TestPipHTMLRewriteAndRawCache(t *testing.T) {
	// 假上游：/simple/<pkg>/ 返回含根相对与绝对两类链接的索引页
	var serverURL string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/simple/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<a href="/packages/aa/foo-1.0.whl#sha256=x">f</a> <a href="%s/packages/bb/bar-2.0.whl#sha256=y">b</a>`, serverURL)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "WHEEL-BYTES")
	}))
	defer up.Close()
	serverURL = up.URL

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"pip": {Upstreams: []config.UpstreamConfig{{BaseURL: up.URL}}},
	})

	w := do(r, http.MethodGet, "/pip/simple/requests/", nil)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/pip/packages/aa/foo-1.0.whl#sha256=x"`) {
		t.Fatalf("根相对链接未被重写:\n%s", body)
	}
	if !strings.Contains(body, `href="/pip/packages/bb/bar-2.0.whl#sha256=y"`) {
		t.Fatalf("绝对链接未被重写:\n%s", body)
	}

	// 缓存中保存的必须是"未重写"的原始字节（HIT 时按当前 Host 重新重写）
	// 桶 = 文件名("requests")前 2 字符 → re
	raw, err := os.ReadFile(filepath.Join(dir, "pip", "re", "simple", "requests~"))
	if err != nil {
		t.Fatalf("索引页应被缓存: %v", err)
	}
	if strings.Contains(string(raw), "/pip/packages") {
		t.Fatalf("缓存应保存上游原始内容:\n%s", raw)
	}
	if !strings.Contains(string(raw), "/packages/aa/foo-1.0.whl") {
		t.Fatalf("缓存内容应包含原始链接:\n%s", raw)
	}

	// 第二次请求（HIT）也应重写
	w = do(r, http.MethodGet, "/pip/simple/requests/", nil)
	if w.Header().Get("X-Mirror-Cache") != "HIT" || !strings.Contains(w.Body.String(), "/pip/packages/aa/foo-1.0.whl") {
		t.Fatalf("HIT 重写失败: %s", w.Body.String())
	}
}

func TestNpmJSONRewrite(t *testing.T) {
	var serverURL string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":"left-pad","versions":{"1.0.0":{"dist":{"tarball":"%s/left-pad/-/left-pad-1.0.0.tgz"}}}}`, serverURL)
	}))
	defer up.Close()
	serverURL = up.URL

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"npm": {Upstreams: []config.UpstreamConfig{{BaseURL: up.URL}}},
	})

	req := httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil)
	req.Host = "mirror.local:8080"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	want := `"tarball": "http://mirror.local:8080/npm/left-pad/-/left-pad-1.0.0.tgz"`
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("JSON tarball 未按 Host 重写:\n%s", w.Body.String())
	}
	// 缓存保持上游原始 JSON
	raw, err := os.ReadFile(filepath.Join(dir, "npm", "le", "left-pad~"))
	if err != nil || strings.Contains(string(raw), "mirror.local") {
		t.Fatalf("缓存应保存原始 JSON: %v", err)
	}
}

func TestScanReloadAcrossRestart(t *testing.T) {
	up := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "persisted")
	})
	defer up.close()

	dir := t.TempDir()
	srcs := map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{BaseURL: up.server.URL}}},
	}
	// 第一个实例：回源并缓存
	_, r1, _ := buildProxy(t, dir, srcs)
	if w := do(r1, http.MethodGet, "/apt/ubuntu/dists/jammy/Release", nil); w.Code != 200 {
		t.Fatalf("首次: %d", w.Code)
	}
	if up.total() != 1 {
		t.Fatalf("上游请求数 = %d, 期望 1", up.total())
	}

	// 第二个实例（模拟重启）：扫描后命中，无需回源
	_, r2, store2 := buildProxy(t, dir, srcs)
	if _, err := store2.Scan(); err != nil {
		t.Fatal(err)
	}
	w := do(r2, http.MethodGet, "/apt/ubuntu/dists/jammy/Release", nil)
	if w.Code != 200 || w.Body.String() != "persisted" || w.Header().Get("X-Mirror-Cache") != "HIT" {
		t.Fatalf("重启后未命中缓存: code=%d body=%q", w.Code, w.Body.String())
	}
	if up.total() != 1 {
		t.Fatalf("重启后不应回源, 上游请求数 = %d", up.total())
	}
}

func TestTTLStaleWhileRevalidate(t *testing.T) {
	var version int64
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		v := version
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		fmt.Fprintf(w, "v%d", v)
	}))
	defer up.Close()

	dir := t.TempDir()
	ttl := config.Duration{Duration: 60 * time.Millisecond}
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {
			Upstreams:     []config.UpstreamConfig{{BaseURL: up.URL}},
			IndexTTL:      ttl,
			IndexPatterns: []string{"^dists/"},
		},
	})

	if w := do(r, http.MethodGet, "/apt/dists/jammy/Release", nil); w.Code != 200 || w.Body.String() != "v0" {
		t.Fatalf("首次: code=%d body=%q", w.Code, w.Body.String())
	}

	time.Sleep(100 * time.Millisecond) // 过期

	// 上游内容已更新为 v1：过期请求先拿到旧数据 v0（优先本地）
	mu.Lock()
	version = 1
	mu.Unlock()
	w := do(r, http.MethodGet, "/apt/dists/jammy/Release", nil)
	if w.Code != 200 || w.Body.String() != "v0" {
		t.Fatalf("过期后应先返回本地旧数据: code=%d body=%q", w.Code, w.Body.String())
	}

	// 等待后台再校验完成后，应拿到 v1
	deadline := time.Now().Add(3 * time.Second)
	for {
		w = do(r, http.MethodGet, "/apt/dists/jammy/Release", nil)
		if w.Body.String() == "v1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("后台再校验未生效, body=%q", w.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 之后稳定为 v1（304 续期，不再变回 v0）
	for i := 0; i < 3; i++ {
		if w := do(r, http.MethodGet, "/apt/dists/jammy/Release", nil); w.Body.String() != "v1" {
			t.Fatalf("内容回退: %q", w.Body.String())
		}
		time.Sleep(70 * time.Millisecond)
	}
}

func TestRangePassthrough(t *testing.T) {
	var sawRange bool
	up := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range") != ""
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "partial")
	})
	defer up.close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{BaseURL: up.server.URL}}},
	})
	w := do(r, http.MethodGet, "/apt/ubuntu/big.deb", map[string]string{"Range": "bytes=0-99"})
	if w.Code != 206 || w.Body.String() != "partial" || !sawRange {
		t.Fatalf("Range 应直通: code=%d body=%q sawRange=%v", w.Code, w.Body.String(), sawRange)
	}
	// 不应产生缓存文件
	if _, err := os.Stat(filepath.Join(dir, "apt", "bi", "ubuntu", "big.deb~")); !os.IsNotExist(err) {
		t.Fatal("Range 直通不应写缓存")
	}
}

func TestPurgeAndStats(t *testing.T) {
	up := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "data")
	})
	defer up.close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{BaseURL: up.server.URL}}},
	})
	for _, p := range []string{"/apt/a.deb", "/apt/b.deb"} {
		if w := do(r, http.MethodGet, p, nil); w.Code != 200 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	// /stats
	w := do(r, http.MethodGet, "/stats", nil)
	if w.Code != 200 {
		t.Fatalf("stats code=%d", w.Code)
	}
	var stats map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	sources := stats["sources"].(map[string]any)
	aptStats := sources["apt"].(map[string]any)
	if aptStats["cache_files"].(float64) != 2 {
		t.Fatalf("stats cache_files = %v, 期望 2", aptStats["cache_files"])
	}
	if stats["hits"].(float64) != 0 || stats["misses"].(float64) != 2 {
		t.Fatalf("hits/misses = %v/%v", stats["hits"], stats["misses"])
	}

	// /purge
	purgeReq := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader(`{"type":"apt"}`))
	purgeReq.Header.Set("Content-Type", "application/json")
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, purgeReq)
	if pw.Code != 200 {
		t.Fatalf("purge code=%d body=%s", pw.Code, pw.Body.String())
	}
	var pres map[string]any
	if err := json.Unmarshal(pw.Body.Bytes(), &pres); err != nil {
		t.Fatal(err)
	}
	if pres["removed"].(float64) != 2 {
		t.Fatalf("removed = %v, 期望 2", pres["removed"])
	}
	// 清空后再请求 → MISS 重新回源
	if w := do(r, http.MethodGet, "/apt/a.deb", nil); w.Header().Get("X-Mirror-Cache") != "MISS" {
		t.Fatal("purge 后应重新回源")
	}
	if up.total() != 3 {
		t.Fatalf("上游请求数 = %d, 期望 3", up.total())
	}
	// healthz
	if w := do(r, http.MethodGet, "/healthz", nil); w.Code != 200 {
		t.Fatal("healthz 应返回 200")
	}
}

func TestBadRequestPath(t *testing.T) {
	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{BaseURL: "http://up.invalid"}}},
	})
	w := do(r, http.MethodGet, "/apt/../../etc/passwd", nil)
	// gin 会先做路由清洗，最坏情况返回 404/400，绝不能 200
	if w.Code == 200 {
		t.Fatalf("路径穿越请求不应成功: %d %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodGet, "/apt/", nil)
	if w.Code != 404 {
		t.Fatalf("空路径应 404: %d", w.Code)
	}
}

func TestStreamedLargeFile(t *testing.T) {
	// 大文件（超出 MaxRewriteSize=1MiB）应走流式路径并完整缓存
	payload := strings.Repeat("A", 3<<20)
	up := newCountingUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, payload)
	})
	defer up.close()

	dir := t.TempDir()
	_, r, _ := buildProxy(t, dir, map[string]*config.SourceConfig{
		"apt": {Upstreams: []config.UpstreamConfig{{BaseURL: up.server.URL}}},
	})
	w := do(r, http.MethodGet, "/apt/ubuntu/pool/main/g/gcc-12.deb", nil)
	if w.Code != 200 || w.Body.Len() != len(payload) {
		t.Fatalf("流式响应错误: code=%d len=%d", w.Code, w.Body.Len())
	}
	bs, err := os.ReadFile(filepath.Join(dir, "apt", "gc", "ubuntu", "pool", "main", "g", "gcc-12.deb~"))
	if err != nil || len(bs) != len(payload) {
		t.Fatalf("大文件未完整缓存: err=%v len=%d", err, len(bs))
	}
	// 第二次命中
	w = do(r, http.MethodGet, "/apt/ubuntu/pool/main/g/gcc-12.deb", nil)
	if w.Header().Get("X-Mirror-Cache") != "HIT" || w.Body.Len() != len(payload) {
		t.Fatal("大文件二次请求应命中")
	}
	if up.total() != 1 {
		t.Fatalf("上游请求数 = %d, 期望 1", up.total())
	}
}
