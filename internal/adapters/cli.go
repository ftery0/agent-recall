package adapters

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func ReadInput(path string) ([]byte, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		r = f
	}
	raw, e := io.ReadAll(io.LimitReader(r, 64<<20+1))
	if len(raw) > 64<<20 {
		return nil, errors.New("input exceeds 64 MiB")
	}
	return raw, e
}

func Home() string {
	if v := os.Getenv("HARNESS_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local/share/harness")
}
