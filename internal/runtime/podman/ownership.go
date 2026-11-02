package podman

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liquidcatmofu/mcwarden/internal/app"
)

var ownershipProperties = []string{
	"--user", "show", "--property=Id", "--property=LoadState",
	"--property=UnitFileState", "--property=SourcePath", "--property=FragmentPath",
	"--property=DropInPaths", "--property=NeedDaemonReload", "--property=ControlGroup",
}

// unitOwnership validates the manager's loaded Quadlet unit before mutation.
// systemctl's rendered ExecStart argv[] is lossy (space-joined), so never parse
// it as an argv vector. Instead read the generated fragment, require that it is
// still loaded/current and has no drop-ins, and parse only a conservative subset
// of systemd command syntax. Unverifiable/custom layouts fail closed.
func (r *Runtime) unitOwnership(ctx context.Context) (string, error) {
	args := append(append([]string(nil), ownershipProperties...), r.cfg.Unit)
	out, err := r.call(ctx, "systemctl", args...)
	if err != nil {
		return "", err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", external("invalid systemd ownership response")
		}
		if _, duplicate := values[key]; duplicate {
			return "", external("duplicate systemd ownership property")
		}
		values[key] = value
	}
	for _, property := range ownershipProperties[2:] {
		if _, ok := values[strings.TrimPrefix(property, "--property=")]; !ok {
			return "", external("incomplete systemd ownership response")
		}
	}
	if values["LoadState"] != "loaded" {
		return "", external("systemd unit is not loaded")
	}
	if values["Id"] != r.cfg.Unit || values["UnitFileState"] != "generated" ||
		!filepath.IsAbs(values["SourcePath"]) || !strings.HasSuffix(values["SourcePath"], ".container") {
		return "", fmt.Errorf("unit is not the configured Quadlet container service: %w", app.ErrConfig)
	}
	if values["NeedDaemonReload"] != "no" || values["DropInPaths"] != "" {
		return "", fmt.Errorf("Quadlet ownership requires a current generated unit without systemd drop-ins: %w", app.ErrUnsupported)
	}
	fragment := values["FragmentPath"]
	if !filepath.IsAbs(fragment) || filepath.Base(fragment) != r.cfg.Unit {
		return "", fmt.Errorf("unverifiable generated unit fragment: %w", app.ErrStateUnknown)
	}
	f, err := os.Open(fragment)
	if err != nil {
		return "", fmt.Errorf("cannot read generated unit fragment: %w", app.ErrStateUnknown)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return "", fmt.Errorf("cannot read generated unit fragment: %w", app.ErrStateUnknown)
	}
	argv, err := generatedStart(data)
	if err != nil {
		return "", err
	}
	if len(argv) < 4 || !filepath.IsAbs(argv[0]) || filepath.Base(argv[0]) != "podman" || argv[1] != "run" {
		return "", fmt.Errorf("unit does not directly run a supported Podman container: %w", app.ErrUnsupported)
	}
	// Quadlet puts --name first, before image/application arguments. Do not
	// search for a matching string in arbitrary arguments or environment values.
	name := ""
	next := 3
	if argv[2] == "--name" {
		name = argv[3]
		next = 4
	} else if strings.HasPrefix(argv[2], "--name=") {
		name = strings.TrimPrefix(argv[2], "--name=")
	}
	if name != r.cfg.Container {
		return "", fmt.Errorf("unit/container ownership mismatch: %w", app.ErrConfig)
	}
	for _, arg := range argv[next:] {
		if arg == "--name" || strings.HasPrefix(arg, "--name=") {
			return "", fmt.Errorf("ambiguous container name in generated unit: %w", app.ErrConfig)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return values["ControlGroup"], nil
}

// generatedStart intentionally rejects continuations, command prefixes,
// expansion and quoting forms that cannot be verified without systemd's parser.
// This is not a general-purpose systemd unit parser.
func generatedStart(data []byte) ([]string, error) {
	unsupported := fmt.Errorf("unsupported generated ExecStart syntax: %w", app.ErrUnsupported)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	section := ""
	var commands []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasSuffix(line, `\`) {
			return nil, unsupported
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if section == "[Service]" && ok && strings.TrimSpace(key) == "ExecStart" {
			value = strings.TrimSpace(value)
			if value == "" {
				commands = nil
			} else {
				commands = append(commands, value)
			}
		}
	}
	if scanner.Err() != nil || len(commands) != 1 {
		return nil, unsupported
	}
	text := commands[0]
	var argv []string
	for text != "" {
		text = strings.TrimLeft(text, " \t")
		if text == "" {
			break
		}
		var word string
		if text[0] == '"' {
			end := 1
			for end < len(text) {
				if text[end] == '\\' {
					end += 2
					continue
				}
				if text[end] == '"' {
					break
				}
				end++
			}
			if end >= len(text) {
				return nil, unsupported
			}
			var err error
			word, err = strconv.Unquote(text[:end+1])
			if err != nil {
				return nil, unsupported
			}
			text = text[end+1:]
			if text != "" && text[0] != ' ' && text[0] != '\t' {
				return nil, unsupported
			}
		} else {
			end := strings.IndexAny(text, " \t")
			if end < 0 {
				end = len(text)
			}
			word, text = text[:end], text[end:]
			if strings.ContainsAny(word, "\"'\\;") {
				return nil, unsupported
			}
		}
		if strings.ContainsAny(word, "$\x00\r\n") {
			return nil, unsupported
		}
		argv = append(argv, word)
	}
	return argv, nil
}

// A running generation must actually be inside the service's delegated cgroup,
// not merely have the right name or a user-supplied label. No PID is an identity.
func containerOwnership(group string, info inspection) error {
	if group == "/" || !path.IsAbs(group) || path.Clean(group) != group ||
		!path.IsAbs(info.CgroupPath) || path.Clean(info.CgroupPath) != info.CgroupPath ||
		!strings.HasPrefix(info.CgroupPath, group+"/") {
		return fmt.Errorf("container is not owned by the configured unit cgroup: %w", app.ErrStateUnknown)
	}
	return nil
}
