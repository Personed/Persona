package skills

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	yaml "gopkg.in/yaml.v3"
)

// Front-matter limits shared with packages/agent/src/work/limits.ts.
const (
	// FileBytes bounds one SKILL.md.
	FileBytes = 256 * 1024
	// NameLength bounds a skill name.
	NameLength = 64
	// DescriptionLength bounds a skill description.
	DescriptionLength = 1024
)

// Bundle is one parsed skill package, as skillPackage in
// packages/agent/src/work/skill-package.ts: the SKILL.md content, its parsed
// front matter, the supporting files and the package hash.
type Bundle struct {
	Content     string
	Name        string
	Description string
	Files       map[string][]byte
	Paths       []string
	Hash        string
}

// packageBundle parses a SKILL.md and an optional base64 ZIP of supporting
// files into a Bundle, as skillPackage in skill-package.ts.
func packageBundle(content, archive string) (Bundle, error) {
	var files map[string][]byte
	if archive != "" {
		if len(archive) > PackageBytes*2 {
			return Bundle{}, &Error{BadRequest, "技能包超过 8 MiB。"}
		}
		raw, err := base64.StdEncoding.DecodeString(archive)
		if err != nil {
			return Bundle{}, &Error{BadRequest, "技能包格式无效。"}
		}
		if files, err = unzip(raw); err != nil {
			return Bundle{}, err
		}
	} else {
		files = map[string][]byte{}
	}
	if archive != "" {
		roots := 0
		var prefix string
		for name := range files {
			if name == "SKILL.md" || strings.HasSuffix(name, "/SKILL.md") {
				roots++
				prefix = name[:len(name)-len("SKILL.md")]
			}
		}
		if _, ok := files["SKILL.md"]; !ok && roots == 1 {
			names := make([]string, 0, len(files))
			for name := range files {
				if !strings.HasPrefix(name, prefix) {
					return Bundle{}, &Error{BadRequest, "ZIP 只能包含一个技能目录。"}
				}
				names = append(names, name)
			}
			for _, name := range names {
				files[name[len(prefix):]] = files[name]
				delete(files, name)
			}
		}
	}
	if content != "" {
		files["SKILL.md"] = []byte(content)
	}
	skillFile, ok := files["SKILL.md"]
	if !ok {
		return Bundle{}, &Error{BadRequest, "缺少 SKILL.md。"}
	}
	text := string(skillFile)
	parsed, err := parseSkill(text)
	if err != nil {
		return Bundle{}, err
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		if !validPath(name) {
			return Bundle{}, &Error{BadRequest, "技能路径无效。"}
		}
		paths = append(paths, name)
	}
	slices.Sort(paths)
	if len(paths) > PackageFiles {
		return Bundle{}, &Error{BadRequest, "技能包最多包含 256 个文件。"}
	}
	hash := sha256.New()
	size := 0
	for _, name := range paths {
		data := files[name]
		size += len(data)
		if size > PackageBytes {
			return Bundle{}, &Error{BadRequest, "技能包超过 8 MiB。"}
		}
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write([]byte(strconv.Itoa(len(data))))
		hash.Write([]byte{0})
		hash.Write(data)
	}
	return Bundle{
		Content: text, Name: parsed.name, Description: parsed.description,
		Files: files, Paths: paths, Hash: fmt.Sprintf("%x", hash.Sum(nil)),
	}, nil
}

// unzip reads an archive's file entries within the package bounds, as the
// fflate filter in skillPackage.
func unzip(raw []byte) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, &Error{BadRequest, "技能包格式无效。"}
	}
	files := map[string][]byte{}
	var bytes int64
	for index, entry := range reader.File {
		if index >= archiveEntries || bytes+int64(entry.UncompressedSize64) > PackageBytes {
			return nil, &Error{BadRequest, "技能文件过大。"}
		}
		if strings.HasSuffix(entry.Name, "/") {
			continue
		}
		data, err := readEntry(entry, PackageBytes-bytes)
		if err != nil {
			return nil, err
		}
		bytes += int64(len(data))
		files[entry.Name] = data
	}
	return files, nil
}

// parsed is the front matter of a SKILL.md, as parseSkill in skill-format.ts.
type parsed struct {
	name, description, body string
}

var frontMatter = regexp.MustCompile(`^\x{FEFF}?---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)([\s\S]*)$`)

// parseSkill validates a SKILL.md: BOM-tolerant `---` YAML front matter with a
// name and description, then a non-empty body.
func parseSkill(content string) (parsed, error) {
	if len(content) > FileBytes {
		return parsed{}, &Error{BadRequest, "SKILL.md超过256 KiB。"}
	}
	match := frontMatter.FindStringSubmatch(content)
	if match == nil {
		return parsed{}, &Error{BadRequest, "SKILL.md需要YAML前置信息。"}
	}
	// Failsafe-schema parity: scalars beyond name/description stay unread, and
	// yaml.v3 already rejects duplicate keys like uniqueKeys.
	var front any
	if err := yaml.Unmarshal([]byte(match[1]), &front); err != nil {
		return parsed{}, &Error{BadRequest, "技能YAML格式无效。"}
	}
	fields, ok := front.(map[string]any)
	if !ok {
		return parsed{}, &Error{BadRequest, "技能前置信息必须是对象。"}
	}
	name, _ := fields["name"].(string)
	if !namePattern.MatchString(name) || len(name) > NameLength {
		return parsed{}, &Error{BadRequest, "技能名须为1至64位小写字母、数字或单连字符。"}
	}
	description, _ := fields["description"].(string)
	if strings.TrimSpace(description) == "" || len(description) > DescriptionLength {
		return parsed{}, &Error{BadRequest, "技能描述须为1至1024个字符。"}
	}
	if strings.TrimSpace(match[2]) == "" {
		return parsed{}, &Error{BadRequest, "技能正文不能为空。"}
	}
	return parsed{name: name, description: strings.TrimSpace(description), body: match[2]}, nil
}

// textOf decodes a package file for the preview, as the TypeScript `text`
// helper: printable text at most 64 KiB, otherwise null.
func textOf(data []byte) *string {
	if data == nil || len(data) > 64*1024 || bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	if !utf8.Valid(data) {
		return nil
	}
	text := string(data)
	return &text
}
