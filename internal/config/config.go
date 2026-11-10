// Package config 加载并校验 mirror-proxy 的 YAML 配置。
//
// 支持的配置结构见项目根目录 config.yaml；每种源类型（apt/yum/pip/npm/mvn）
// 可配置多个上游，回源时按配置顺序依次尝试。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 包装 time.Duration，使 YAML 中可以写 "30m"、"6h" 等人类可读形式。
type Duration struct {
	time.Duration
}

// UnmarshalYAML 实现 yaml.Unmarshaler，接受字符串（如 "6h"）或纳秒整数。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		if strings.TrimSpace(s) == "" {
			d.Duration = 0
			return nil
		}
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("无法解析时长 %q: %w", s, err)
		}
		d.Duration = v
		return nil
	}
	var n int64
	if err := value.Decode(&n); err == nil {
		d.Duration = time.Duration(n)
		return nil
	}
	return fmt.Errorf("非法时长配置（第 %d 行）", value.Line)
}

// UpstreamConfig 单个上游源站配置。
type UpstreamConfig struct {
	// Name 上游名称，用于日志与 /stats 统计；缺省时取 base_url 的 host。
	Name string `yaml:"name"`
	// BaseURL 上游基础 URL（含路径前缀，如 https://mirrors.aliyun.com/ubuntu）。
	BaseURL string `yaml:"base_url"`
	// Timeout 针对该上游的单次请求总超时；0 表示使用 server.upstream_timeout。
	Timeout Duration `yaml:"timeout"`
}

// SourceConfig 单个源类型（apt/yum/pip/npm/mvn 或自定义）的配置。
type SourceConfig struct {
	// Route 对外暴露的路由前缀，默认 "/<类型名>"，如 /apt、/pip。
	Route string `yaml:"route"`
	// Upstreams 上游列表，按顺序故障转移；至少 1 个。
	Upstreams []UpstreamConfig `yaml:"upstreams"`
	// Rewrite 是否对返回的 HTML/JSON 做链接重写（pip/npm 默认开启，其余默认关闭）。
	Rewrite *bool `yaml:"rewrite"`
	// IndexTTL 索引类文件（如 apt 的 dists/、yum 的 repodata/、npm 元数据）
	// 的再校验周期；0 表示永不回源校验（纯"随传随存"模式）。
	IndexTTL Duration `yaml:"index_ttl"`
	// IndexPatterns 哪些请求路径视为"索引文件"（正则，RE2 语法）。
	// 缺省时使用各类型的内置默认规则。
	IndexPatterns []string `yaml:"index_patterns"`
}

// ServerConfig 服务与全局行为配置。
type ServerConfig struct {
	// Addr 监听地址，默认 ":8080"。
	Addr string `yaml:"addr"`
	// CacheDir 缓存根目录，默认 ".cache"。
	CacheDir string `yaml:"cache_dir"`
	// PublicBaseURL 对外基础 URL（如 http://mirror.local:8080），
	// 用于 JSON 链接重写；缺省时按请求 Host 自动推断。
	PublicBaseURL string `yaml:"public_base_url"`
	// MaxRewriteSize 允许整体读入内存做链接重写的响应体上限（字节），默认 64MiB。
	MaxRewriteSize int64 `yaml:"max_rewrite_size"`
	// ResponseHeaderTimeout 等待上游响应头的超时，默认 30s。
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
	// UpstreamTimeout 所有上游的单请求默认总超时；0 表示不限制（仍受响应头超时约束）。
	UpstreamTimeout Duration `yaml:"upstream_timeout"`
}

// Config 顶层配置。
type Config struct {
	Server  ServerConfig             `yaml:"server"`
	Sources map[string]*SourceConfig `yaml:"sources"`
}

var (
	typeRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	routeRe = regexp.MustCompile(`^/[A-Za-z0-9_-]+$`)
)

// Load 从 path 读取并校验配置，随后应用环境变量覆盖。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 配置: %w", err)
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		return nil, err
	}
	cfg.ApplyEnv()
	return cfg, nil
}

// ApplyEnv 应用环境变量覆盖：
//
//	MIRROR_PROXY_ADDR      -> server.addr
//	MIRROR_PROXY_CACHE_DIR -> server.cache_dir
func (c *Config) ApplyEnv() {
	if v := os.Getenv("MIRROR_PROXY_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("MIRROR_PROXY_CACHE_DIR"); v != "" {
		c.Server.CacheDir = v
	}
}

func (c *Config) normalizeAndValidate() error {
	sc := &c.Server
	if sc.Addr == "" {
		sc.Addr = ":8080"
	}
	if sc.CacheDir == "" {
		sc.CacheDir = ".cache"
	}
	if sc.MaxRewriteSize <= 0 {
		sc.MaxRewriteSize = 64 << 20
	}
	if sc.ResponseHeaderTimeout.Duration <= 0 {
		sc.ResponseHeaderTimeout = Duration{Duration: 30 * time.Second}
	}
	if sc.UpstreamTimeout.Duration < 0 {
		return errors.New("server.upstream_timeout 不允许为负数")
	}

	if len(c.Sources) == 0 {
		return errors.New("sources 不能为空，至少需要配置一个源类型（apt/yum/pip/npm/mvn）")
	}

	seenRoutes := map[string]string{}
	for name, s := range c.Sources {
		if !typeRe.MatchString(name) {
			return fmt.Errorf("非法的源类型名 %q：仅允许小写字母、数字与中划线", name)
		}
		if s == nil {
			return fmt.Errorf("sources.%s 配置为空：至少需要 1 个 upstream", name)
		}
		if len(s.Upstreams) == 0 {
			return fmt.Errorf("sources.%s.upstreams 不能为空", name)
		}
		if s.Route == "" {
			s.Route = "/" + name
		}
		if !routeRe.MatchString(s.Route) {
			return fmt.Errorf("sources.%s.route 非法（%q）：形如 /apt，不含尾斜杠", name, s.Route)
		}
		if other, dup := seenRoutes[s.Route]; dup {
			return fmt.Errorf("路由冲突：%s 与 %s 都使用了 %s", other, name, s.Route)
		}
		seenRoutes[s.Route] = name

		for i := range s.Upstreams {
			u := &s.Upstreams[i]
			if strings.TrimSpace(u.BaseURL) == "" {
				return fmt.Errorf("sources.%s.upstreams[%d].base_url 为空", name, i)
			}
			pu, err := url.Parse(strings.TrimSpace(u.BaseURL))
			if err != nil || pu.Scheme == "" || pu.Host == "" {
				return fmt.Errorf("sources.%s.upstreams[%d].base_url 非法: %q", name, i, u.BaseURL)
			}
			if pu.Scheme != "http" && pu.Scheme != "https" {
				return fmt.Errorf("sources.%s.upstreams[%d].base_url 仅支持 http/https: %q", name, i, u.BaseURL)
			}
			if u.Name == "" {
				u.Name = pu.Host
			}
			u.BaseURL = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
			if u.Timeout.Duration < 0 {
				return fmt.Errorf("sources.%s.upstreams[%d].timeout 不允许为负数", name, i)
			}
		}
	}
	return nil
}

// SourceConfig 便捷方法：判断是否启用链接重写（未配置时按类型默认）。
func (s *SourceConfig) RewriteEnabled(typeName string) bool {
	if s.Rewrite != nil {
		return *s.Rewrite
	}
	return typeName == "pip" || typeName == "npm"
}
