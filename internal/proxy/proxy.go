// Package proxy 实现镜像代理的核心逻辑：
//
//   - 本地优先：请求先查内存索引（启动时由磁盘扫描重建），命中直接回盘；
//   - 未命中：按 YAML 配置顺序逐个尝试上游，首个成功者边下发客户端边写入
//     本地缓存（随传随存）；网络错误 / 5xx / 404 时自动故障转移到下一上游；
//   - 索引类文件（apt dists/、yum repodata/、pip simple、npm 元数据等）可
//     配置 TTL：过期后先返回本地旧数据（stale-while-revalidate），后台带
//     If-Modified-Since 再校验，304 则续期、200 则原子替换。
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"mirror-proxy/internal/cache"
	"mirror-proxy/internal/config"
)

// ---------------------------------------------------------------------------
// 类型定义
// ---------------------------------------------------------------------------

type upstreamInfo struct {
	name    string
	base    *url.URL
	baseStr string        // 无尾斜杠的字符串形式，用于拼接 URL
	timeout time.Duration // 单次请求总超时；0 = 使用全局配置
}

// Source 运行期的源定义。
type Source struct {
	name      string
	route     string
	upstreams []upstreamInfo
	rewrite   bool
	ttl       time.Duration
	indexRes  []*regexp.Regexp
}

// matchIndex 判断请求路径是否为"索引文件"（需要 TTL 再校验）。
func (st *Source) matchIndex(rest string) bool {
	for _, re := range st.indexRes {
		if re.MatchString(rest) {
			return true
		}
	}
	return false
}

// deriveCT 扫描恢复的缓存条目没有 Content-Type，按规则推断。
func (st *Source) deriveCT(rest string) string {
	switch st.name {
	case "pip":
		if st.matchIndex(rest) {
			return "text/html; charset=utf-8"
		}
	case "npm":
		if st.matchIndex(rest) {
			return "application/json; charset=utf-8"
		}
	}
	if ext := strings.ToLower(filepath.Ext(rest)); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return "application/octet-stream"
}

// Proxy 镜像代理。
type Proxy struct {
	cfg     *config.Config
	store   *cache.Store
	log     *slog.Logger
	client  *http.Client
	sources []*Source
	flights *flightGroup
	stats   *Stats

	byName map[string]*Source
}

// fetchError 回源失败。wrote 为 true 表示已经向客户端写出（部分）响应，
// 调用方不能再写第二份错误响应。
type fetchError struct {
	wrote  bool
	status int
	msg    string
}

func (e *fetchError) Error() string { return e.msg }

type fetchResult struct{ wrote bool }

type refreshDone struct{}

// ---------------------------------------------------------------------------
// 构造
// ---------------------------------------------------------------------------

// 各源类型的默认索引规则（可被 YAML index_patterns 覆盖）。
var defaultIndexPatterns = map[string][]string{
	"apt": {`^dists/`},
	"yum": {`(?:^|/)repodata/`},
	"pip": {`^simple/`},
	"npm": {`^@[^/]+/[^/]+$`, `^[^/@][^/]*$`},
	"mvn": {`(?:^|/)maven-metadata\.xml$`},
}

// New 构建代理实例。
func New(cfg *config.Config, store *cache.Store, log *slog.Logger) (*Proxy, error) {
	if log == nil {
		log = slog.Default()
	}
	transport := &http.Transport{
		Proxy:                 nil, // 上游直连，不走系统代理
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: cfg.Server.ResponseHeaderTimeout.Duration,
		ForceAttemptHTTP2:     true,
	}
	p := &Proxy{
		cfg:     cfg,
		store:   store,
		log:     log,
		client:  &http.Client{Transport: transport}, // 总超时由每请求 context 控制
		sources: []*Source{},
		flights: newFlightGroup(),
		stats:   newStats(),
		byName:  map[string]*Source{},
	}

	for typeName, sc := range cfg.Sources {
		st, err := buildSource(typeName, sc, cfg)
		if err != nil {
			return nil, err
		}
		p.sources = append(p.sources, st)
		p.byName[typeName] = st
	}
	return p, nil
}

func buildSource(typeName string, sc *config.SourceConfig, cfg *config.Config) (*Source, error) {
	st := &Source{
		name:    typeName,
		route:   sc.Route,
		rewrite: sc.RewriteEnabled(typeName),
		ttl:     sc.IndexTTL.Duration,
	}
	if st.route == "" {
		st.route = "/" + typeName
	}
	for _, uc := range sc.Upstreams {
		u, err := url.Parse(uc.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("sources.%s: 上游 %s URL 非法: %w", typeName, uc.Name, err)
		}
		timeout := uc.Timeout.Duration
		if timeout == 0 {
			timeout = cfg.Server.UpstreamTimeout.Duration
		}
		st.upstreams = append(st.upstreams, upstreamInfo{
			name:    uc.Name,
			base:    u,
			baseStr: strings.TrimRight(uc.BaseURL, "/"),
			timeout: timeout,
		})
	}
	patterns := sc.IndexPatterns
	if len(patterns) == 0 {
		patterns = defaultIndexPatterns[typeName]
	}
	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("sources.%s.index_patterns 非法 %q: %w", typeName, pat, err)
		}
		st.indexRes = append(st.indexRes, re)
	}
	return st, nil
}

// Register 在 gin 引擎上注册代理路由与运维接口。
func (p *Proxy) Register(r *gin.Engine) {
	for _, st := range p.sources {
		st := st
		r.GET(st.route+"/*path", func(c *gin.Context) { p.handle(c, st) })
		r.HEAD(st.route+"/*path", func(c *gin.Context) { p.handle(c, st) })
	}
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/stats", p.handleStats)
	r.POST("/purge", p.handlePurge)
}

// ---------------------------------------------------------------------------
// 请求处理
// ---------------------------------------------------------------------------

func (p *Proxy) handle(c *gin.Context, st *Source) {
	segs, err := cache.CleanSegments(strings.TrimPrefix(c.Param("path"), "/"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(segs) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "路径为空",
			"hint":  "请请求具体文件，例如 " + st.route + "/ubuntu/dists/jammy/InRelease",
		})
		return
	}
	rest := strings.Join(segs, "/")

	// Range 请求直接透传上游（不缓存，避免存储部分内容）。
	if c.Request.Header.Get("Range") != "" {
		p.passthrough(c, st, rest)
		return
	}

	if e, ok := p.store.Lookup(st.name, rest); ok {
		p.stats.hits.Add(1)
		if p.expired(st, rest, e) {
			// 先把本地数据返回给客户端（优先本地），后台再校验上游。
			p.stats.staleServed.Add(1)
			p.serveEntry(c, st, e)
			p.refreshAsync(st, rest, e)
			return
		}
		p.serveEntry(c, st, e)
		return
	}

	// 未命中：HEAD 无法缓存，透传即可。
	if c.Request.Method == http.MethodHead {
		p.stats.misses.Add(1)
		p.passthrough(c, st, rest)
		return
	}
	p.stats.misses.Add(1)

	key := cache.Key(st.name, rest)
	_, ferr, shared := p.flights.Do(key, func() (any, error) {
		return p.fetchOrigin(c, st, rest)
	})
	if ferr != nil {
		var fe *fetchError
		if errors.As(ferr, &fe) && fe.wrote {
			p.log.Warn("回源中断（客户端已收到部分响应）", "source", st.name, "rest", rest, "error", fe.msg)
			return
		}
		p.respondError(c, st, ferr)
		return
	}
	if !shared {
		return // 首个请求者已在 fetchOrigin 中完成响应
	}
	// 并发等待者：flight 已完成且缓存已写入，从缓存补发。
	if e, ok := p.store.Lookup(st.name, rest); ok {
		p.stats.hits.Add(1)
		p.serveEntry(c, st, e)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "回源完成但缓存条目缺失", "rest": rest})
}

func (p *Proxy) respondError(c *gin.Context, st *Source, err error) {
	status := http.StatusBadGateway
	msg := err.Error()
	var fe *fetchError
	if errors.As(err, &fe) && fe.status != 0 {
		status = fe.status
	}
	c.Header("X-Mirror-Upstreams-Tried", strconv.Itoa(len(st.upstreams)))
	c.JSON(status, gin.H{
		"error":  msg,
		"source": st.name,
		"hint":   "所有上游均失败；请检查 /stats 中的 upstreams 计数与上游可用性",
	})
}

// ---------------------------------------------------------------------------
// 回源
// ---------------------------------------------------------------------------

// fetchUpstream 向指定上游发起请求。返回 cancel 须由调用方在响应体
// 读取完毕后调用（用于带超时的 context）。
func (p *Proxy) fetchUpstream(ctx context.Context, method string, srcReq *http.Request,
	up upstreamInfo, rest string, ifModSince time.Time,
) (*http.Response, context.CancelFunc, error) {
	u := up.baseStr + "/" + rest
	if srcReq != nil && srcReq.URL.RawQuery != "" {
		u += "?" + srcReq.URL.RawQuery
	}
	reqCtx := ctx
	var cancel context.CancelFunc = func() {}
	if up.timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, up.timeout)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, u, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if srcReq != nil {
		if ua := srcReq.Header.Get("User-Agent"); ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if ac := srcReq.Header.Get("Accept"); ac != "" {
			req.Header.Set("Accept", ac)
		}
		if rg := srcReq.Header.Get("Range"); rg != "" {
			req.Header.Set("Range", rg)
		}
	}
	// 强制 identity：既保证缓存的是原始字节，也避免压缩体无法重写链接。
	req.Header.Set("Accept-Encoding", "identity")
	if !ifModSince.IsZero() {
		req.Header.Set("If-Modified-Since", ifModSince.UTC().Format(http.TimeFormat))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// fetchOrigin 未命中时的回源主流程：按顺序尝试上游，边下发边缓存。
// 注意使用独立于客户端请求的 context：客户端中途断开仍要完成缓存写入。
func (p *Proxy) fetchOrigin(c *gin.Context, st *Source, rest string) (*fetchResult, error) {
	ctx := context.Background()
	for _, up := range st.upstreams {
		resp, cancel, err := p.fetchUpstream(ctx, http.MethodGet, c.Request, up, rest, time.Time{})
		if err != nil {
			p.stats.upErr(up.name)
			p.log.Debug("上游请求失败，尝试下一个", "source", st.name, "upstream", up.name, "error", err.Error())
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			p.stats.upErr(up.name)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			cancel()
			p.log.Debug("上游返回失败状态，尝试下一个", "source", st.name, "upstream", up.name, "status", resp.StatusCode)
			continue
		}
		p.stats.upOK(up.name)
		if resp.StatusCode != http.StatusOK {
			// 其他状态（3xx 残留 / 4xx）：原样转发，不缓存。
			p.relayResponse(c, resp)
			resp.Body.Close()
			cancel()
			return &fetchResult{wrote: true}, nil
		}
		serveErr := p.serveAndCache(c, st, rest, up, resp)
		resp.Body.Close()
		cancel()
		if serveErr == nil {
			return &fetchResult{wrote: true}, nil
		}
		var fe *fetchError
		if errors.As(serveErr, &fe) {
			return nil, fe // 已向客户端写出（或不可重试）
		}
		p.log.Debug("读取上游响应失败，尝试下一个", "source", st.name, "upstream", up.name, "error", serveErr.Error())
	}
	return nil, &fetchError{
		status: http.StatusNotFound,
		msg:    fmt.Sprintf("所有 %d 个上游均不可用或未找到该文件", len(st.upstreams)),
	}
}

// serveAndCache 处理一个 200 响应：小体积可重写内容整体缓冲后重写下发；
// 其余内容流式下发并同时写入缓存。
//
// 返回：nil=完成；*fetchError=已向客户端写出；普通 error=未写出、可重试下一上游。
func (p *Proxy) serveAndCache(c *gin.Context, st *Source, rest string, up upstreamInfo, resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	lm := parseHTTPDate(resp.Header.Get("Last-Modified"))
	body := io.Reader(resp.Body)
	contentLength := resp.ContentLength

	switch ce := resp.Header.Get("Content-Encoding"); ce {
	case "", "identity":
	case "gzip":
		// 上游忽略 identity 仍返回 gzip：透明解压后缓存与下发。
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("上游返回非法 gzip 数据: %w", err)
		}
		body = gz
		contentLength = -1
	default:
		p.relayResponse(c, resp)
		return &fetchError{wrote: true, status: resp.StatusCode, msg: "unsupported Content-Encoding: " + ce}
	}

	maxRewrite := p.cfg.Server.MaxRewriteSize
	if st.rewrite && rewritableCT(ct) && (contentLength < 0 || contentLength <= maxRewrite) {
		data, readErr := io.ReadAll(io.LimitReader(body, maxRewrite+1))
		if readErr == nil {
			if int64(len(data)) <= maxRewrite {
				if _, saveErr := p.store.SaveBytes(st.name, rest, data, lm, ct); saveErr != nil {
					p.log.Warn("缓存写入失败，本次仅直接服务", "source", st.name, "rest", rest, "error", saveErr.Error())
				}
				out := p.rewriteBody(c, st, data, ct)
				c.Header("X-Mirror-Cache", "MISS")
				c.Header("X-Mirror-Upstream", up.name)
				c.Data(http.StatusOK, ct, out)
				return nil
			}
			// 超过重写上限：流式路径继续（先写出已读前缀）。
			return p.streamOut(c, st, rest, up, body, ct, -1, lm, data)
		}
		// 缓冲阶段失败：尚未向客户端写出任何内容，可安全重试下一上游。
		return fmt.Errorf("读取上游响应失败: %w", readErr)
	}
	return p.streamOut(c, st, rest, up, body, ct, contentLength, lm, nil)
}

// teeWriter 把上游数据同时写入缓存文件与客户端；客户端断开的错误被吞掉，
// 以保证缓存仍能写完（"随传随存"）。
type teeWriter struct {
	part    *os.File
	client  *clientWriter
	total   int64
	aborted bool
}

func (t *teeWriter) Write(b []byte) (int, error) {
	n, err := t.part.Write(b)
	t.total += int64(n)
	if err != nil {
		t.aborted = true
		return n, err
	}
	_, _ = t.client.Write(b) // 错误已被吞掉
	return len(b), nil
}

// clientWriter 包装 gin 响应写：客户端断开时记录错误并假装成功。
type clientWriter struct {
	w   gin.ResponseWriter
	err error
}

func (cw *clientWriter) Write(b []byte) (int, error) {
	if cw.err != nil {
		return len(b), nil
	}
	n, err := cw.w.Write(b)
	if err != nil {
		cw.err = err
		return len(b), nil
	}
	return n, nil
}

// streamOut 流式下发并写缓存。
func (p *Proxy) streamOut(c *gin.Context, st *Source, rest string, up upstreamInfo,
	body io.Reader, ct string, contentLength int64, lm time.Time, prefix []byte,
) error {
	r := body
	if len(prefix) > 0 {
		r = io.MultiReader(bytes.NewReader(prefix), body)
	}
	part, err := p.store.NewPart(st.name, rest)
	if err != nil {
		// 磁盘不可写：放弃缓存，直接转发。
		p.log.Warn("无法创建缓存文件，本次不缓存", "source", st.name, "rest", rest, "error", err.Error())
		return p.relayRaw(c, r, ct, contentLength, lm)
	}

	if ct != "" {
		c.Header("Content-Type", ct)
	}
	if contentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	if !lm.IsZero() {
		c.Header("Last-Modified", lm.UTC().Format(http.TimeFormat))
	}
	c.Header("X-Mirror-Cache", "MISS")
	c.Header("X-Mirror-Upstream", up.name)
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()

	tee := &teeWriter{part: part.File(), client: &clientWriter{w: c.Writer}}
	_, copyErr := io.CopyBuffer(tee, r, make([]byte, 256<<10))
	if copyErr != nil {
		part.Abort()
		p.log.Error("缓存写入中止（响应已部分发出）", "source", st.name, "rest", rest, "error", copyErr.Error())
		return &fetchError{wrote: true, status: http.StatusOK, msg: "cache write aborted: " + copyErr.Error()}
	}
	if _, err := part.Commit(tee.total, lm, ct); err != nil {
		p.log.Warn("缓存提交失败", "source", st.name, "rest", rest, "error", err.Error())
	}
	return nil
}

// relayRaw 直接把流转发给客户端（不落缓存）。
func (p *Proxy) relayRaw(c *gin.Context, r io.Reader, ct string, contentLength int64, lm time.Time) error {
	if ct != "" {
		c.Header("Content-Type", ct)
	}
	if contentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	if !lm.IsZero() {
		c.Header("Last-Modified", lm.UTC().Format(http.TimeFormat))
	}
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()
	_, _ = io.CopyBuffer(c.Writer, r, make([]byte, 64<<10))
	return nil
}

// relayResponse 原样转发上游响应（透传路径：Range / HEAD / 异常状态码）。
func (p *Proxy) relayResponse(c *gin.Context, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	if resp.ContentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		c.Header("Last-Modified", lm)
	}
	if et := resp.Header.Get("ETag"); et != "" {
		c.Header("ETag", et)
	}
	c.Status(resp.StatusCode)
	c.Writer.WriteHeaderNow()
	if c.Request.Method != http.MethodHead {
		_, _ = io.CopyBuffer(c.Writer, resp.Body, make([]byte, 64<<10))
	}
}

// passthrough 不经过缓存的直通转发（Range / 未命中 HEAD）。
func (p *Proxy) passthrough(c *gin.Context, st *Source, rest string) {
	ctx := c.Request.Context()
	for _, up := range st.upstreams {
		resp, cancel, err := p.fetchUpstream(ctx, c.Request.Method, c.Request, up, rest, time.Time{})
		if err != nil {
			p.stats.upErr(up.name)
			continue
		}
		if resp.StatusCode >= 500 {
			p.stats.upErr(up.name)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			cancel()
			continue
		}
		p.stats.upOK(up.name)
		p.relayResponse(c, resp)
		resp.Body.Close()
		cancel()
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "所有上游均不可用", "source": st.name})
}

// ---------------------------------------------------------------------------
// 缓存命中路径
// ---------------------------------------------------------------------------

func (p *Proxy) serveEntry(c *gin.Context, st *Source, e cache.Entry) {
	ct := e.ContentType
	if ct == "" {
		ct = st.deriveCT(e.Rest)
	}
	if st.rewrite && rewritableCT(ct) && e.Size <= p.cfg.Server.MaxRewriteSize {
		if data, err := os.ReadFile(e.Path); err == nil {
			out := p.rewriteBody(c, st, data, ct)
			c.Header("X-Mirror-Cache", "HIT")
			c.Data(http.StatusOK, ct, out)
			return
		}
		// 读取失败（文件被外部删除等）：回退到回源。
	}

	f, err := os.Open(e.Path)
	if err != nil {
		p.log.Warn("缓存文件丢失，直接透传上游", "source", st.name, "rest", e.Rest, "error", err.Error())
		p.passthrough(c, st, e.Rest)
		return
	}
	defer f.Close()
	c.Header("Content-Type", ct)
	c.Header("X-Mirror-Cache", "HIT")
	// ServeContent 免费提供 Range / If-Modified-Since / HEAD 支持。
	http.ServeContent(c.Writer, c.Request, filepath.Base(e.Path), e.ModTime, f)
}

// expired 判断是否需要再校验（stale-while-revalidate 的触发条件）。
func (p *Proxy) expired(st *Source, rest string, e cache.Entry) bool {
	if st.ttl <= 0 || e.ModTime.IsZero() {
		return false
	}
	if !st.matchIndex(rest) {
		return false
	}
	return time.Since(e.ModTime) > st.ttl
}

// ---------------------------------------------------------------------------
// TTL 再校验（后台）
// ---------------------------------------------------------------------------

func (p *Proxy) refreshAsync(st *Source, rest string, e cache.Entry) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				p.log.Error("后台再校验 panic", "recover", r)
			}
		}()
		_, err, _ := p.flights.Do(cache.Key(st.name, rest), func() (any, error) {
			return p.refreshEntry(st, rest, e)
		})
		if err != nil {
			p.log.Debug("再校验失败，继续使用本地旧数据", "source", st.name, "rest", rest, "error", err.Error())
		}
	}()
}

// refreshEntry 带 If-Modified-Since 的条件回源：304 续期；200 原子替换缓存。
func (p *Proxy) refreshEntry(st *Source, rest string, e cache.Entry) (any, error) {
	ctx := context.Background()
	for _, up := range st.upstreams {
		resp, cancel, err := p.fetchUpstream(ctx, http.MethodGet, nil, up, rest, e.ModTime)
		if err != nil {
			p.stats.upErr(up.name)
			continue
		}
		switch resp.StatusCode {
		case http.StatusNotModified:
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			cancel()
			p.store.Touch(st.name, rest)
			p.stats.refresh304.Add(1)
			return refreshDone{}, nil
		case http.StatusOK:
			part, perr := p.store.NewPart(st.name, rest)
			if perr == nil {
				n, cerr := io.CopyBuffer(part.File(), resp.Body, make([]byte, 256<<10))
				if cerr == nil {
					_, _ = part.Commit(n, parseHTTPDate(resp.Header.Get("Last-Modified")), resp.Header.Get("Content-Type"))
					p.stats.refresh200.Add(1)
				} else {
					part.Abort()
				}
			}
			resp.Body.Close()
			cancel()
			return refreshDone{}, nil
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			cancel()
		}
	}
	return nil, &fetchError{status: http.StatusBadGateway, msg: "revalidate: 所有上游均失败"}
}

// ---------------------------------------------------------------------------
// 链接重写
// ---------------------------------------------------------------------------

func rewritableCT(ct string) bool {
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/json")
}

func (p *Proxy) rewriteBody(c *gin.Context, st *Source, data []byte, ct string) []byte {
	if !st.rewrite {
		return data
	}
	if strings.Contains(ct, "text/html") {
		return RewriteHTML(data, st.route, st.upstreams)
	}
	if strings.Contains(ct, "application/json") {
		return RewriteJSON(data, st.route, p.absBase(c), st.upstreams)
	}
	return data
}

// absBase 代理对外绝对地址（JSON 重写需要）。
func (p *Proxy) absBase(c *gin.Context) string {
	if pb := p.cfg.Server.PublicBaseURL; pb != "" {
		return strings.TrimRight(pb, "/")
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

// ---------------------------------------------------------------------------
// 运维接口
// ---------------------------------------------------------------------------

func (p *Proxy) handleStats(c *gin.Context) {
	sources := gin.H{}
	for _, st := range p.sources {
		cs := p.store.TypeStats(st.name)
		names := make([]string, 0, len(st.upstreams))
		for _, u := range st.upstreams {
			names = append(names, u.name)
		}
		sources[st.name] = gin.H{
			"route":       st.route,
			"upstreams":   names,
			"rewrite":     st.rewrite,
			"index_ttl":   st.ttl.String(),
			"cache_files": cs.Files,
			"cache_bytes": cs.Bytes,
		}
	}
	ups := gin.H{}
	for name, uc := range p.stats.snapshotUps() {
		ups[name] = gin.H{"ok": uc.OK, "err": uc.Err}
	}
	c.JSON(http.StatusOK, gin.H{
		"hits":          p.stats.hits.Load(),
		"misses":        p.stats.misses.Load(),
		"stale_served":  p.stats.staleServed.Load(),
		"refresh_304":   p.stats.refresh304.Load(),
		"refresh_200":   p.stats.refresh200.Load(),
		"in_flight":     p.flights.InFlight(),
		"cache_entries": p.store.Count(),
		"sources":       sources,
		"upstreams":     ups,
	})
}

func (p *Proxy) handlePurge(c *gin.Context) {
	var req struct {
		Type   string `json:"type" binding:"required"`
		Prefix string `json:"prefix"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体须为 {\"type\": \"apt\", \"prefix\": \"可选路径前缀\"}"})
		return
	}
	st, ok := p.byName[req.Type]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未配置的源类型: " + req.Type})
		return
	}
	removed, freed, err := p.store.PurgePrefix(req.Type, req.Prefix)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p.log.Info("清理缓存", "source", req.Type, "prefix", req.Prefix, "removed", removed, "freed", freed)
	c.JSON(http.StatusOK, gin.H{
		"type":        req.Type,
		"prefix":      req.Prefix,
		"route":       st.route,
		"removed":     removed,
		"bytes_freed": freed,
	})
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func parseHTTPDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := http.ParseTime(s); err == nil {
		return t
	}
	return time.Time{}
}
