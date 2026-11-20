package worker

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type listRequest struct {
	Path       string   `json:"path"`
	Hidden     *bool    `json:"hidden,omitempty"`
	Gitignore  *bool    `json:"gitignore,omitempty"`
	Exclude    []string `json:"exclude,omitempty"`
	MaxEntries int      `json:"maxEntries,omitempty"`
	MaxBytes   int      `json:"maxBytes,omitempty"`
	TimeoutMs  int      `json:"timeoutMs,omitempty"`
}

type listResult struct {
	Count     int  `json:"count"`
	Truncated bool `json:"truncated"`
}

var errListFull = errors.New("list full")

// listFiles walks the tree below request.Path depth first, siblings in byte order,
// without following symbolic links. The payload holds one record per entry: a kind
// byte (f, d, l), the relative slash path, and a NUL byte.
func listFiles(ctx context.Context, request listRequest) (listResult, []byte, error) {
	root := filepath.Clean(request.Path)
	info, err := os.Stat(root)
	if err != nil {
		return listResult{}, nil, err
	}
	if !info.IsDir() {
		return listResult{}, nil, codeError("ENOTDIR", "not a directory", root)
	}
	maxEntries := request.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 50_000
	}
	maxBytes := request.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	maxBytes = min(maxBytes, MaxFrame-64<<10)
	timeout := time.Duration(request.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	walker := &listWalker{
		ctx: ctx, root: root, deadline: deadline, maxEntries: maxEntries, maxBytes: maxBytes,
		hidden: request.Hidden == nil || *request.Hidden, exclude: map[string]bool{".git": true},
	}
	for _, name := range request.Exclude {
		walker.exclude[name] = true
	}
	var ignores []ignoreSet
	if request.Gitignore == nil || *request.Gitignore {
		ignores = repositoryIgnores(root)
	}
	err = walker.walk(root, "", ignores)
	result := listResult{Count: walker.count, Truncated: errors.Is(err, errListFull)}
	if err != nil && !result.Truncated {
		return listResult{}, nil, err
	}
	return result, walker.payload, nil
}

type listWalker struct {
	ctx        context.Context
	root       string
	deadline   time.Time
	maxEntries int
	maxBytes   int
	hidden     bool
	exclude    map[string]bool
	count      int
	payload    []byte
}

func (w *listWalker) walk(dir, relative string, ignores []ignoreSet) error {
	if ignores != nil {
		ignores = append(ignores, readIgnoreFile(filepath.Join(dir, ".gitignore"), dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if dir == w.root {
			return err
		}
		return nil
	}
	for _, entry := range entries {
		if err := w.ctx.Err(); err != nil {
			return codeError("aborted", "listing cancelled", w.root)
		}
		if time.Now().After(w.deadline) {
			return errListFull
		}
		name := entry.Name()
		if w.exclude[name] || (!w.hidden && strings.HasPrefix(name, ".")) {
			continue
		}
		kind := byte(0)
		switch mode := entry.Type(); {
		case mode.IsRegular():
			kind = 'f'
		case mode.IsDir():
			kind = 'd'
		case mode&os.ModeSymlink != 0:
			kind = 'l'
		}
		if kind == 0 {
			continue
		}
		path := filepath.Join(dir, name)
		if ignored(ignores, path, kind == 'd') {
			continue
		}
		child := name
		if relative != "" {
			child = relative + "/" + name
		}
		child = strings.ToValidUTF8(child, string(utf8.RuneError))
		if w.count >= w.maxEntries || len(w.payload)+len(child)+2 > w.maxBytes {
			return errListFull
		}
		w.payload = append(append(append(w.payload, kind), child...), 0)
		w.count++
		if kind == 'd' {
			if err := w.walk(path, child, ignores); err != nil {
				return err
			}
		}
	}
	return nil
}

type ignoreRule struct {
	pattern *regexp.Regexp
	negate  bool
	dirOnly bool
}

type ignoreSet struct {
	base  string
	rules []ignoreRule
}

func ignored(sets []ignoreSet, path string, dir bool) bool {
	result := false
	for _, set := range sets {
		relative, err := filepath.Rel(set.base, path)
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
			continue
		}
		relative = filepath.ToSlash(relative)
		for _, rule := range set.rules {
			if (!rule.dirOnly || dir) && rule.pattern.MatchString(relative) {
				result = !rule.negate
			}
		}
	}
	return result
}

// repositoryIgnores returns the global excludes, .git/info/exclude and the
// .gitignore files of root's parents inside its git work tree, lowest precedence
// first. Outside a work tree it returns nil, which disables ignore rules.
func repositoryIgnores(root string) []ignoreSet {
	top := root
	for {
		if _, err := os.Stat(filepath.Join(top, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(top)
		if parent == top {
			return nil
		}
		top = parent
	}
	sets := []ignoreSet{}
	if global := globalExcludesFile(); global != "" {
		sets = append(sets, readIgnoreFile(global, top))
	}
	sets = append(sets, readIgnoreFile(filepath.Join(top, ".git", "info", "exclude"), top))
	var parents []string
	for dir := root; dir != top; {
		dir = filepath.Dir(dir)
		parents = append([]string{dir}, parents...)
	}
	for _, dir := range parents {
		sets = append(sets, readIgnoreFile(filepath.Join(dir, ".gitignore"), dir))
	}
	return sets
}

func globalExcludesFile() string {
	if config := os.Getenv("XDG_CONFIG_HOME"); config != "" {
		return filepath.Join(config, "git", "ignore")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

func readIgnoreFile(path, base string) ignoreSet {
	set := ignoreSet{base: base}
	file, err := os.Open(path)
	if err != nil {
		return set
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if rule, ok := parseIgnoreRule(scanner.Text()); ok {
			set.rules = append(set.rules, rule)
		}
	}
	return set
}

// parseIgnoreRule compiles one gitignore line, following gitignore(5).
func parseIgnoreRule(line string) (ignoreRule, bool) {
	line = strings.TrimRight(strings.TrimSuffix(line, "\r"), " ")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	var rule ignoreRule
	if strings.HasPrefix(line, "!") {
		rule.negate, line = true, line[1:]
	} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		rule.dirOnly, line = true, strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	var expression strings.Builder
	if !anchored {
		expression.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case strings.HasPrefix(line[i:], "**/"):
			expression.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(line[i:], "/**") && i+3 == len(line):
			expression.WriteString("/.*")
			i += 2
		case strings.HasPrefix(line[i:], "**"):
			expression.WriteString(".*")
			i++
		case c == '*':
			expression.WriteString("[^/]*")
		case c == '?':
			expression.WriteString("[^/]")
		case c == '\\' && i+1 < len(line):
			i++
			expression.WriteString(regexp.QuoteMeta(line[i : i+1]))
		case c == '[':
			end := strings.IndexByte(line[i+1:], ']')
			if end < 0 {
				expression.WriteString(`\[`)
				continue
			}
			class := line[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		default:
			expression.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	pattern, err := regexp.Compile("^" + expression.String() + "$")
	if err != nil {
		return ignoreRule{}, false
	}
	rule.pattern = pattern
	return rule, true
}
