package memory

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Object = map[string]any

var ErrBusy = errors.New("writer is busy")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,159}$`)

func Identifier(s string) error {
	if !idPattern.MatchString(s) {
		return fmt.Errorf("invalid identifier %q", s)
	}
	return nil
}
func Text(v any) string { s, _ := v.(string); return s }
func Number(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return -1
		}
		return i
	}
	return -1
}
func Decode(b []byte) (Object, error) {
	var v Object
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	err := d.Decode(&v)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if v == nil && err == nil {
		err = errors.New("object required")
	}
	return v, err
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

// Historical IDs use Python json.dumps(ensure_ascii=False, sort_keys=True).
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	var emit func(any) error
	emit = func(v any) error {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			b.WriteString(strconv.FormatBool(x))
		case string:
			b.WriteByte('"')
			for _, r := range x {
				switch r {
				case '"', '\\':
					b.WriteByte('\\')
					b.WriteRune(r)
				case '\b':
					b.WriteString(`\b`)
				case '\f':
					b.WriteString(`\f`)
				case '\n':
					b.WriteString(`\n`)
				case '\r':
					b.WriteString(`\r`)
				case '\t':
					b.WriteString(`\t`)
				default:
					if r < 32 {
						fmt.Fprintf(&b, `\u%04x`, r)
					} else {
						b.WriteRune(r)
					}
				}
			}
			b.WriteByte('"')
		case json.Number:
			s := x.String()
			if strings.ContainsAny(s, ".eE") {
				f, e := x.Float64()
				if e != nil {
					return e
				}
				s = strconv.FormatFloat(f, 'f', -1, 64)
				if f != 0 && (f >= 1e16 || f <= -1e16 || (f < 1e-4 && f > -1e-4)) {
					s = strconv.FormatFloat(f, 'e', -1, 64)
				} else if !strings.Contains(s, ".") {
					s += ".0"
				}
			} else {
				n, ok := new(big.Int).SetString(s, 10)
				if !ok {
					return fmt.Errorf("invalid integer %s", s)
				}
				s = n.String()
			}
			b.WriteString(s)
		case int:
			fmt.Fprint(&b, x)
		case int64:
			fmt.Fprint(&b, x)
		case []any:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteString(", ")
				}
				if e := emit(item); e != nil {
					return e
				}
			}
			b.WriteByte(']')
		case []string:
			a := make([]any, len(x))
			for i, s := range x {
				a[i] = s
			}
			return emit(a)
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteString(", ")
				}
				_ = emit(k)
				b.WriteString(": ")
				if e := emit(x[k]); e != nil {
					return e
				}
			}
			b.WriteByte('}')
		default:
			raw, e := json.Marshal(v)
			if e != nil {
				return e
			}
			var normalized any
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			if e = d.Decode(&normalized); e != nil {
				return e
			}
			return emit(normalized)
		}
		return nil
	}
	err := emit(v)
	return b.Bytes(), err
}
func Packed(v any) []byte {
	b, e := Canonical(v)
	if e != nil {
		panic(e)
	}
	return b
}
func PrivateWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".harness-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func Lock(path string) (func(), error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			f.Close()
			return nil, e
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func Resolve(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		path = filepath.Join(home, path[2:])
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	parent := absolute
	var missing []string
	for {
		resolved, e := filepath.EvalSymlinks(parent)
		if e == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", e
		}
		missing = append(missing, filepath.Base(parent))
		parent = next
	}
}
func Within(root, path string) bool {
	r, e := Resolve(root)
	if e != nil {
		return false
	}
	p, e := Resolve(path)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(r, p)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func NewOutput(root, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute output path required")
	}
	if Within(root, path) {
		return errors.New("output overlaps memory data")
	}
	if _, e := os.Lstat(path); !os.IsNotExist(e) {
		return errors.New("output already exists or is inaccessible")
	}
	ext := filepath.Ext(path)
	if ext != ".md" && ext != ".txt" && ext != ".json" {
		return errors.New("unsupported output extension")
	}
	return nil
}

func WriteNew(path string, raw []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(raw); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
