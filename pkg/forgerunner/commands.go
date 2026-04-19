package forgerunner

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Command is a parsed workflow command from process stdout.
//
//	::name param=val,param2=val2::message
type Command struct {
	Name   string
	Params map[string]string
	Value  string
}

// ParseCommand extracts a workflow command from a log line.
// Returns nil, false if the line is not a command.
func ParseCommand(line string) (*Command, bool) {
	if !strings.HasPrefix(line, "::") {
		return nil, false
	}
	// Find the closing ::
	rest := line[2:]
	idx := strings.Index(rest, "::")
	if idx < 0 {
		return nil, false
	}

	head := rest[:idx]
	value := rest[idx+2:]

	// Split head into name and params: "name param1=v1,param2=v2"
	name := head
	params := map[string]string{}
	if spaceIdx := strings.IndexByte(head, ' '); spaceIdx >= 0 {
		name = head[:spaceIdx]
		paramStr := head[spaceIdx+1:]
		for _, kv := range strings.Split(paramStr, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if ok {
				params[k] = v
			}
		}
	}

	if name == "" {
		return nil, false
	}

	return &Command{Name: name, Params: params, Value: value}, true
}

// FileCommands holds values read from GITHUB_OUTPUT, GITHUB_ENV, and
// GITHUB_PATH file-based command files after a step executes.
type FileCommands struct {
	Outputs  map[string]string // from GITHUB_OUTPUT
	EnvVars  map[string]string // from GITHUB_ENV
	PathAdds []string          // from GITHUB_PATH
}

// ParseFileCommands reads the file-based command files written by a step.
func ParseFileCommands(outputPath, envPath, pathPath string) (*FileCommands, error) {
	fc := &FileCommands{
		Outputs: map[string]string{},
		EnvVars: map[string]string{},
	}

	if outputPath != "" {
		kvs, err := parseKeyValueFile(outputPath)
		if err != nil {
			return nil, fmt.Errorf("parse GITHUB_OUTPUT: %w", err)
		}
		fc.Outputs = kvs
	}

	if envPath != "" {
		kvs, err := parseKeyValueFile(envPath)
		if err != nil {
			return nil, fmt.Errorf("parse GITHUB_ENV: %w", err)
		}
		fc.EnvVars = kvs
	}

	if pathPath != "" {
		lines, err := readNonEmptyLines(pathPath)
		if err != nil {
			return nil, fmt.Errorf("parse GITHUB_PATH: %w", err)
		}
		fc.PathAdds = lines
	}

	return fc, nil
}

// parseKeyValueFile reads a file of key=value pairs and heredoc blocks:
//
//	key=value
//	key<<EOF
//	multi-line
//	value
//	EOF
func parseKeyValueFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]string{}, nil
	}

	result := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	for scanner.Scan() {
		line := scanner.Text()

		// Heredoc: key<<DELIM
		if k, delim, ok := strings.Cut(line, "<<"); ok {
			k = strings.TrimSpace(k)
			delim = strings.TrimSpace(delim)
			var sb strings.Builder
			for scanner.Scan() {
				hline := scanner.Text()
				if hline == delim {
					break
				}
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(hline)
			}
			result[k] = sb.String()
			continue
		}

		// Simple: key=value
		if k, v, ok := strings.Cut(line, "="); ok {
			k = strings.TrimSpace(k)
			if k != "" {
				result[k] = v
			}
		}
	}
	return result, scanner.Err()
}

func readNonEmptyLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// SecretMasker replaces registered secret values with "***" in log output.
type SecretMasker struct {
	secrets []string
}

// NewSecretMasker creates a masker with the given secrets.
// Empty and short (< 3 char) secrets are ignored to avoid false positives.
func NewSecretMasker(secrets []string) *SecretMasker {
	var filtered []string
	for _, s := range secrets {
		if len(s) >= 3 {
			filtered = append(filtered, s)
		}
	}
	return &SecretMasker{secrets: filtered}
}

// AddSecret registers an additional secret for masking.
func (m *SecretMasker) AddSecret(s string) {
	if len(s) >= 3 {
		m.secrets = append(m.secrets, s)
	}
}

// Mask replaces all registered secrets in s with "***".
func (m *SecretMasker) Mask(s string) string {
	for _, secret := range m.secrets {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}
