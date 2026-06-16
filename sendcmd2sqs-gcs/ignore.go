package main

import (
	"bufio"
	"os"
	"strings"

	"github.com/gobwas/glob"
)

// ignoreMatcher 持有从 .ignore-gcs 编译出的一组 glob 规则。
type ignoreMatcher struct {
	globs []glob.Glob
}

// globCompile 以 '/' 作为路径分隔符编译规则，使 '*' 不跨目录、'**' 跨目录
// （近似 gitignore/gitwildmatch 语义）。
func globCompile(pattern string) (glob.Glob, error) {
	return glob.Compile(pattern, '/')
}

// loadIgnorePatterns 加载并编译 .ignore-gcs 中的过滤规则。
// 文件不存在则返回空匹配器（不过滤）。读取/编译失败降级为不过滤，不中断迁移。
func loadIgnorePatterns() *ignoreMatcher {
	m := &ignoreMatcher{}
	f, err := os.Open(IgnoreFile)
	if err != nil {
		if os.IsNotExist(err) {
			logf("未找到 %s 文件，将不进行过滤", IgnoreFile)
		} else {
			logf("打开 %s 失败: %v，将不进行过滤", IgnoreFile, err)
		}
		return m
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// inventory 行可能很长，放大 scanner 缓冲。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		g, err := globCompile(line)
		if err != nil {
			logf("跳过无效的 ignore 规则 %q: %v", line, err)
			continue
		}
		m.globs = append(m.globs, g)
	}
	if err := sc.Err(); err != nil {
		logf("读取 %s 出错: %v，已加载 %d 条规则", IgnoreFile, err, len(m.globs))
		return m
	}
	logf("已加载 %s 过滤规则（%d 条）", IgnoreFile, len(m.globs))
	return m
}

// shouldSkip 判断 object name 是否命中任一 ignore 规则（命中则跳过）。
func (m *ignoreMatcher) shouldSkip(name string) bool {
	for _, g := range m.globs {
		if g.Match(name) {
			return true
		}
	}
	return false
}
