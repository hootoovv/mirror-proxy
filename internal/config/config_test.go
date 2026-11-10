package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const sampleYAML = `
server:
  addr: ":9090"
  cache_dir: "/tmp/mp-cache"
  max_rewrite_size: 8388608
  response_header_timeout: 15s

sources:
  apt:
    upstreams:
      - name: aliyun-ubuntu
        base_url: https://mirrors.aliyun.com/ubuntu
      - base_url: https://mirrors.tuna.tsinghua.edu.cn/ubuntu
    index_ttl: 6h
    index_patterns:
      - "^dists/"
  pip:
    upstreams:
      - base_url: https://mirrors.aliyun.com/pypi
    rewrite: false
  npm:
    upstreams:
      - base_url: https://registry.npmmirror.com
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9090" || cfg.Server.CacheDir != "/tmp/mp-cache" {
		t.Fatalf("server 解析错误: %+v", cfg.Server)
	}
	if cfg.Server.MaxRewriteSize != 8388608 {
		t.Fatalf("max_rewrite_size = %d", cfg.Server.MaxRewriteSize)
	}
	if cfg.Server.ResponseHeaderTimeout.Duration != 15*time.Second {
		t.Fatalf("response_header_timeout = %v", cfg.Server.ResponseHeaderTimeout)
	}

	apt := cfg.Sources["apt"]
	if apt == nil {
		t.Fatal("缺少 apt 源")
	}
	if apt.Route != "/apt" {
		t.Fatalf("apt.route 默认值错误: %q", apt.Route)
	}
	if len(apt.Upstreams) != 2 {
		t.Fatal("apt 上游数量错误")
	}
	// 第二个上游未配置 name → 默认取 host
	if apt.Upstreams[1].Name != "mirrors.tuna.tsinghua.edu.cn" {
		t.Fatalf("上游默认 name 错误: %q", apt.Upstreams[1].Name)
	}
	if apt.IndexTTL.Duration != 6*time.Hour {
		t.Fatalf("index_ttl = %v", apt.IndexTTL)
	}
	if len(apt.IndexPatterns) != 1 || apt.IndexPatterns[0] != "^dists/" {
		t.Fatalf("index_patterns = %v", apt.IndexPatterns)
	}
	if apt.RewriteEnabled("apt") {
		t.Fatal("apt 默认不开启链接重写")
	}

	pip := cfg.Sources["pip"]
	if pip.RewriteEnabled("pip") != false {
		t.Fatal("显式 rewrite: false 应关闭重写")
	}
	if !cfg.Sources["npm"].RewriteEnabled("npm") {
		t.Fatal("npm 默认开启链接重写")
	}
	if cfg.Sources["npm"].Route != "/npm" {
		t.Fatalf("npm.route = %q", cfg.Sources["npm"].Route)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"空 sources":   "sources: {}",
		"上游为空":        "sources:\n  apt:\n    upstreams: []",
		"非法 base_url": "sources:\n  apt:\n    upstreams:\n      - base_url: \"ftp://x/y\"",
		"非法时长":        "sources:\n  apt:\n    upstreams:\n      - base_url: http://u\n    index_ttl: \"6 hourz\"",
		"非法路由":        "sources:\n  apt:\n    route: \"apt/\"\n    upstreams:\n      - base_url: http://u",
		"路由冲突":        "sources:\n  apt:\n    upstreams:\n      - base_url: http://u\n  yum:\n    route: /apt\n    upstreams:\n      - base_url: http://v",
		"非法类型名":       "sources:\n  APT:\n    upstreams:\n      - base_url: http://u",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, y)); err == nil {
				t.Fatalf("应当报错: %s", name)
			}
		})
	}
}

func TestDurationParsing(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"6h", 6 * time.Hour},
		{"\"\"", 0},
		{"90s", 90 * time.Second},
		{"1h30m", 90 * time.Minute},
	}
	for _, tc := range cases {
		var d Duration
		if err := d.UnmarshalYAML(mustYAMLNode(t, tc.in)); err != nil {
			t.Fatalf("UnmarshalYAML(%q) 错误: %v", tc.in, err)
		}
		if d.Duration != tc.want {
			t.Fatalf("UnmarshalYAML(%q) = %v, 期望 %v", tc.in, d.Duration, tc.want)
		}
	}
	var d Duration
	if err := d.UnmarshalYAML(mustYAMLNode(t, "abc")); err == nil {
		t.Fatal("非法时长应报错")
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg, err := Load(writeTemp(t, sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRROR_PROXY_ADDR", ":7070")
	t.Setenv("MIRROR_PROXY_CACHE_DIR", "/data/cache")
	cfg.ApplyEnv()
	if cfg.Server.Addr != ":7070" || cfg.Server.CacheDir != "/data/cache" {
		t.Fatalf("环境变量覆盖失败: %+v", cfg.Server)
	}
}

// mustYAMLNode 把 YAML 标量解析为 *yaml.Node（UnmarshalYAML 的入参类型）。
func mustYAMLNode(t *testing.T, val string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(val), &doc); err != nil {
		t.Fatalf("解析 YAML 标量 %q: %v", val, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("空 YAML: %q", val)
	}
	return doc.Content[0]
}
