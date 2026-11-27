package builtin

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/mcp"
)

var (
	_ loader.SkillFiles = (*mcpSkillFiles)(nil)
	_ skillFileReader   = (*mcp.Client)(nil)
)

type skillFileReader interface {
	ReadSkillFile(context.Context, mcp.SkillFile) ([]byte, error)
}

type mcpSkillFiles struct {
	reader   skillFileReader
	dir      string
	document mcp.SkillFile
	files    map[string]mcp.SkillFile
}

func newMCPSkillFiles(dir string, entry mcp.Skill, reader skillFileReader) (*mcpSkillFiles, error) {
	source := &mcpSkillFiles{reader: reader, dir: dir, files: make(map[string]mcp.SkillFile)}
	root := strings.TrimSuffix(entry.URI, "/SKILL.md")

	for _, file := range entry.Files {
		if _, err := mcpSkillPath(dir, file.URI); err != nil {
			return nil, err
		}

		if file.URI == entry.URI {
			source.document = file

			continue
		}

		rel := strings.TrimPrefix(file.URI, root+"/")
		source.files[rel] = file
	}

	return source, nil
}

func (s *mcpSkillFiles) Document(ctx context.Context) ([]byte, error) {
	_, data, err := s.store(ctx, s.document)

	return data, err
}

func (s *mcpSkillFiles) Open(ctx context.Context, name string) (loader.OpenedFile, error) {
	file, ok := s.files[name]
	if !ok {
		file, ok = s.files[html.UnescapeString(name)]
	}

	if !ok {
		return loader.OpenedFile{}, fmt.Errorf("file %s is not listed in the skill's manifest", name)
	}

	path, data, err := s.store(ctx, file)
	if err != nil {
		return loader.OpenedFile{}, err
	}

	return loader.OpenedFile{Path: path, Data: data}, nil
}

func (s *mcpSkillFiles) store(ctx context.Context, file mcp.SkillFile) (string, []byte, error) {
	root := strings.TrimSuffix(s.document.URI, "/SKILL.md")
	rel := strings.TrimPrefix(file.URI, root+"/")

	path, err := mcpSkillPath(s.dir, file.URI)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", rel, err)
	}

	if data, err := os.ReadFile(path); err == nil && file.Verify(data) == nil {
		return path, data, nil
	}

	data, err := s.reader.ReadSkillFile(ctx, file)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", rel, err)
	}

	if err := writeMCPSkillFile(path, data); err != nil {
		return "", nil, fmt.Errorf("%s: %w", rel, err)
	}

	return path, data, nil
}

func writeMCPSkillFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}

	file, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return fmt.Errorf("create skill file: %w", err)
	}

	if err := replaceMCPSkillFile(file, path, data); err != nil {
		_ = os.Remove(file.Name())

		return err
	}

	return nil
}

func replaceMCPSkillFile(file *os.File, path string, data []byte) error {
	defer func() { _ = file.Close() }()

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write skill file: %w", err)
	}

	if err := file.Chmod(0o500); err != nil {
		return fmt.Errorf("set skill file mode: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close skill file: %w", err)
	}

	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace skill file: %w", err)
	}

	return nil
}

func mcpSkillPath(dir, uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("unsafe skill file URI %s", uri)
	}

	if !validMCPSkillURLShape(u, uri) {
		return "", fmt.Errorf("unsafe skill file URI %s", uri)
	}

	segments := []string{u.Scheme, u.Host}
	for escaped := range strings.SplitSeq(strings.TrimPrefix(u.EscapedPath(), "/"), "/") {
		segment, err := url.PathUnescape(escaped)
		if err != nil {
			return "", fmt.Errorf("unsafe skill file URI %s", uri)
		}

		segments = append(segments, segment)
	}

	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\x00/") {
			return "", fmt.Errorf("unsafe skill file URI %s", uri)
		}
	}

	return filepath.Join(append([]string{dir}, segments...)...), nil
}

func validMCPSkillURLShape(u *url.URL, raw string) bool {
	switch {
	case u.Scheme == "":
		return false
	case u.Host == "":
		return false
	case u.User != nil:
		return false
	case u.Port() != "":
		return false
	case u.RawQuery != "":
		return false
	case u.ForceQuery:
		return false
	case strings.Contains(raw, "#"):
		return false
	case u.Opaque != "":
		return false
	default:
		return true
	}
}
