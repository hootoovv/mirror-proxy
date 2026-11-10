package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiskRelPath(t *testing.T) {
	cases := []struct {
		name     string
		typeName string
		rest     string
		want     string
	}{
		{
			name:     "apt Ubuntu 版本分层：jammy 的 Packages.gz",
			typeName: "apt",
			rest:     "ubuntu/dists/jammy/main/binary-amd64/Packages.gz",
			want:     "apt/Pa/ubuntu/dists/jammy/main/binary-amd64/Packages.gz~",
		},
		{
			name:     "apt Ubuntu 版本分层：noble 的同名 Packages.gz 落在不同子目录",
			typeName: "apt",
			rest:     "ubuntu/dists/noble/main/binary-amd64/Packages.gz",
			want:     "apt/Pa/ubuntu/dists/noble/main/binary-amd64/Packages.gz~",
		},
		{
			name:     "apt pool 包文件（版本号中的 ~ 被转义为 ~7E）",
			typeName: "apt",
			rest:     "ubuntu/pool/main/g/gcc-12/gcc-12_12.3.0-1ubuntu1~5_amd64.deb",
			want:     "apt/gc/ubuntu/pool/main/g/gcc-12/gcc-12_12.3.0-1ubuntu1~7E5_amd64.deb~",
		},
		{
			name:     "yum repodata",
			typeName: "yum",
			rest:     "7.9.2009/os/x86_64/repodata/repomd.xml",
			want:     "yum/re/7.9.2009/os/x86_64/repodata/repomd.xml~",
		},
		{
			name:     "pip 扁平 wheel：两级结构",
			typeName: "pip",
			rest:     "numpy-1.26.4-cp311-cp311-manylinux_2_17_x86_64.whl",
			want:     "pip/nu/numpy-1.26.4-cp311-cp311-manylinux_2_17_x86_64.whl~",
		},
		{
			name:     "npm 包元数据：单段路径",
			typeName: "npm",
			rest:     "lodash",
			want:     "npm/lo/lodash~",
		},
		{
			name:     "npm tarball",
			typeName: "npm",
			rest:     "react/-/react-18.2.0.tgz",
			want:     "npm/re/react/-/react-18.2.0.tgz~",
		},
		{
			name:     "npm 作用域包元数据",
			typeName: "npm",
			rest:     "@babel/core",
			want:     "npm/co/@babel/core~",
		},
		{
			name:     "mvn 构件",
			typeName: "mvn",
			rest:     "org/springframework/spring-context/5.3.30/spring-context-5.3.30.jar",
			want:     "mvn/sp/org/springframework/spring-context/5.3.30/spring-context-5.3.30.jar~",
		},
		{
			name:     "单字符文件名：桶为整体",
			typeName: "apt",
			rest:     "x",
			want:     "apt/x/x~",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiskRelPath(tc.typeName, tc.rest)
			if err != nil {
				t.Fatalf("DiskRelPath(%q, %q) 出错: %v", tc.typeName, tc.rest, err)
			}
			// 统一为 "/" 分隔比较，避免 Windows 下 filepath.Join 产生 "\" 导致误报
			if filepath.ToSlash(got) != tc.want {
				t.Fatalf("DiskRelPath(%q, %q) = %q, 期望 %q", tc.typeName, tc.rest, got, tc.want)
			}
		})
	}
}

func TestDiskRelPathEncodesSpecialChars(t *testing.T) {
	rest := "pkg%3F-1.0#tag.whl"
	got, err := DiskRelPath("pip", rest)
	if err != nil {
		t.Fatal(err)
	}
	// 桶 = 文件名前 2 个字符 "pk"，扁平文件名带文件标记后缀
	want := filepath.Join("pip", "pk", encodeSegment(rest)+fileMarker)
	if got != want {
		t.Fatalf("DiskRelPath = %q, 期望 %q", got, want)
	}
	// 编码后不含裸的 ?/# 等特殊字符
	if strings.ContainsAny(got, "?#") {
		t.Fatalf("编码后仍含特殊字符: %q", got)
	}
}

func TestDiskRelPathRejects(t *testing.T) {
	for _, bad := range []struct {
		typeName, rest string
	}{
		{"apt", "../etc/passwd"},
		{"apt", "ubuntu/../../etc/passwd"},
		{"APT", "x"},  // 类型名必须小写
		{"apt", ""},   // 空路径
		{"apt", "./"}, // 清洗后为空
		{"a b", "x"},  // 类型名含空格
	} {
		if _, err := DiskRelPath(bad.typeName, bad.rest); err == nil {
			t.Fatalf("DiskRelPath(%q, %q) 应当报错", bad.typeName, bad.rest)
		}
	}
}

func TestSaveLookupRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello apt cache")
	e, err := s.SaveBytes("apt", "ubuntu/dists/jammy/InRelease", data, time.Time{}, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != int64(len(data)) {
		t.Fatalf("Size = %d, 期望 %d", e.Size, len(data))
	}
	got, ok := s.Lookup("apt", "ubuntu/dists/jammy/InRelease")
	if !ok {
		t.Fatal("Lookup 未命中")
	}
	if got.Path != e.Path {
		t.Fatalf("Path 不一致: %q vs %q", got.Path, e.Path)
	}
	bs, err := os.ReadFile(got.Path)
	if err != nil || string(bs) != string(data) {
		t.Fatalf("磁盘内容不一致: %q, err=%v", bs, err)
	}
	// 期望的磁盘布局
	rel, err := DiskRelPath("apt", "ubuntu/dists/jammy/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), rel)); err != nil {
		t.Fatalf("缓存文件不在期望路径 %s: %v", rel, err)
	}
}

func TestScanRebuildsIndexAndRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	if _, err := s1.SaveBytes("apt", "ubuntu/dists/jammy/Release", []byte("rel-jammy"), time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.SaveBytes("apt", "ubuntu/dists/noble/Release", []byte("rel-noble"), time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.SaveBytes("pip", "numpy-1.0-py3-none-any.whl", []byte("whl"), time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	// 制造一个中断残留的临时文件
	tmpDir := filepath.Join(dir, "pip", "nu")
	if err := os.WriteFile(filepath.Join(tmpDir, tempPrefix+"-123456789"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	s2, _ := New(dir) // 新实例：模拟进程重启（内存索引为空）
	if _, ok := s2.Lookup("apt", "ubuntu/dists/jammy/Release"); ok {
		t.Fatal("重启后、扫描前不应命中")
	}
	stats, err := s2.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if stats["apt"].Files != 2 || stats["pip"].Files != 1 {
		t.Fatalf("扫描统计错误: %+v", stats)
	}
	for _, key := range []struct{ typ, rest string }{
		{"apt", "ubuntu/dists/jammy/Release"},
		{"apt", "ubuntu/dists/noble/Release"},
		{"pip", "numpy-1.0-py3-none-any.whl"},
	} {
		e, ok := s2.Lookup(key.typ, key.rest)
		if !ok {
			t.Fatalf("扫描后 Lookup(%s/%s) 未命中", key.typ, key.rest)
		}
		bs, _ := os.ReadFile(e.Path)
		if key.rest == "ubuntu/dists/jammy/Release" && string(bs) != "rel-jammy" {
			t.Fatalf("jammy 内容错误: %q", bs)
		}
		if key.rest == "ubuntu/dists/noble/Release" && string(bs) != "rel-noble" {
			t.Fatalf("noble 内容错误: %q", bs)
		}
	}
	// 临时文件应被删除
	if _, err := os.Stat(filepath.Join(tmpDir, tempPrefix+"-123456789")); !os.IsNotExist(err) {
		t.Fatal("残留临时文件未被清理")
	}
}

// 回归测试：npm 包元数据（单段路径，缓存为文件）与 tarball（其子路径，
// 需要同名目录）不能在磁盘布局中冲突。修复前元数据先落盘为 <桶>/<pkg>
// 文件后，tarball 的 <桶>/<pkg>/-/ 目录创建会失败（ENOTDIR），导致
// npm tarball 永远无法缓存。
func TestNpmMetadataAndTarballCoexist(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.SaveBytes("npm", "lodash", []byte(`{"name":"lodash"}`), time.Time{}, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	tarball, err := s.SaveBytes("npm", "lodash/-/lodash-4.17.21.tgz", []byte("TARBALL"), time.Time{}, "application/octet-stream")
	if err != nil {
		t.Fatalf("tarball 落盘失败（元数据与 tarball 冲突）: %v", err)
	}
	// 元数据必须是文件，tarball 位于其"同名目录"之下，两者互不覆盖
	if fi, err := os.Stat(meta.Path); err != nil || fi.IsDir() {
		t.Fatalf("元数据应落为文件: %v, err=%v", fi, err)
	}
	if _, err := os.Stat(tarball.Path); err != nil {
		t.Fatalf("tarball 未落盘: %v", err)
	}
	// 模拟重启：扫描后两者都应恢复且内容正确
	s2, _ := New(dir)
	if _, err := s2.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Lookup("npm", "lodash"); !ok {
		t.Fatal("扫描后 npm 元数据丢失")
	}
	e, ok := s2.Lookup("npm", "lodash/-/lodash-4.17.21.tgz")
	if !ok {
		t.Fatal("扫描后 npm tarball 丢失")
	}
	if bs, _ := os.ReadFile(e.Path); string(bs) != "TARBALL" {
		t.Fatalf("tarball 内容错误: %q", bs)
	}
	// 作用域包（@scope/pkg 元数据 vs @scope/pkg/-/x.tgz）同理
	if _, err := s.SaveBytes("npm", "@scope/pkg", []byte("m"), time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBytes("npm", "@scope/pkg/-/pkg-1.0.0.tgz", []byte("t"), time.Time{}, ""); err != nil {
		t.Fatalf("作用域包 tarball 落盘失败: %v", err)
	}
}

func TestPurgePrefix(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	for _, rest := range []string{
		"ubuntu/dists/jammy/Release",
		"ubuntu/dists/noble/Release",
		"ubuntu/pool/main/g/gcc-12.deb",
	} {
		if _, err := s.SaveBytes("apt", rest, []byte("x"), time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	n, freed, err := s.PurgePrefix("apt", "ubuntu/dists/jammy")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || freed != 1 {
		t.Fatalf("PurgePrefix = (%d, %d), 期望 (1, 1)", n, freed)
	}
	if _, ok := s.Lookup("apt", "ubuntu/dists/jammy/Release"); ok {
		t.Fatal("jammy 条目应已删除")
	}
	if _, ok := s.Lookup("apt", "ubuntu/dists/noble/Release"); !ok {
		t.Fatal("noble 条目不应被误删")
	}
	// 清空整个类型
	n, _, err = s.PurgePrefix("apt", "")
	if err != nil || n != 2 {
		t.Fatalf("PurgePrefix(全部) = (%d, err=%v), 期望 2", n, err)
	}
	// 非法输入
	if _, _, err := s.PurgePrefix("apt", "../etc"); err == nil {
		t.Fatal("PurgePrefix 应拒绝包含 .. 的前缀")
	}
}

func TestTouch(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	old := time.Now().Add(-2 * time.Hour).UTC()
	if _, err := s.SaveBytes("apt", "x.deb", []byte("x"), old, ""); err != nil {
		t.Fatal(err)
	}
	if !s.Touch("apt", "x.deb") {
		t.Fatal("Touch 应返回 true")
	}
	e, _ := s.Lookup("apt", "x.deb")
	if time.Since(e.ModTime) > time.Minute {
		t.Fatalf("ModTime 未被刷新: %v", e.ModTime)
	}
	if s.Touch("apt", "不存在") {
		t.Fatal("Touch 不存在的键应返回 false")
	}
}
