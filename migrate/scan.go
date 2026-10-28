package migrate

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// File es un fichero del origen pendiente de evaluar.
type File struct {
	Rel         string // ruta relativa al origen, con separador '/'
	Abs         string // ruta absoluta en disco
	Size        int64  // tamaño en disco (0 en los marcadores "solo online")
	ModTime     int64  // segundos Unix
	Placeholder bool   // marcador de Dropbox "solo online": el contenido no está en disco
}

// placeholderXattr es el atributo extendido con el que el cliente clásico de
// Dropbox para macOS marca los ficheros "solo online" (Smart Sync).
const placeholderXattr = "com.dropbox.placeholder"

// IsPlaceholder indica si un fichero es un marcador de Dropbox sin contenido local.
func IsPlaceholder(path string, size int64) bool {
	if size != 0 {
		return false
	}
	return hasXattr(path, placeholderXattr)
}

// DefaultExcludes son los patrones que se saltan siempre en una carpeta de
// Dropbox en macOS: metadatos de Dropbox, del Finder y ficheros AppleDouble.
var DefaultExcludes = []string{
	".dropbox",
	".dropbox.cache",
	".dropbox.attr",
	".DS_Store",
	"Icon\r",
	"._*",
	"*.tmp",
	"~$*",
}

// ScanOptions configura el recorrido del origen.
type ScanOptions struct {
	SourceDir string
	Excludes  []string // patrones glob aplicados al nombre y a la ruta relativa; con "/" inicial, solo a la ruta exacta
}

// Scan recorre el origen y devuelve los ficheros regulares ordenados por ruta.
// Se omiten enlaces simbólicos y todo lo que case con los patrones de exclusión.
func Scan(opts ScanOptions) ([]File, error) {
	root, err := filepath.Abs(opts.SourceDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("origen: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("origen %s no es un directorio", root)
	}

	excludes := append([]string{}, DefaultExcludes...)
	excludes = append(excludes, opts.Excludes...)

	var files []File
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Carpeta ilegible: se avisa y se continúa con el resto.
			fmt.Fprintf(os.Stderr, "aviso: no se puede leer %s: %v\n", p, walkErr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if matchesAny(excludes, d.Name(), rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "aviso: no se puede inspeccionar %s: %v\n", p, err)
			return nil
		}
		files = append(files, File{
			Rel:         rel,
			Abs:         p,
			Size:        fi.Size(),
			ModTime:     fi.ModTime().Unix(),
			Placeholder: IsPlaceholder(p, fi.Size()),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, nil
}

func matchesAny(patterns []string, name, rel string) bool {
	for _, pat := range patterns {
		if strings.HasPrefix(pat, "/") {
			// Anclado a la ruta relativa completa (es lo que escribe "Saltar").
			pat = strings.TrimPrefix(pat, "/")
			if ok, _ := path.Match(pat, rel); ok {
				return true
			}
			if strings.HasPrefix(rel, strings.TrimSuffix(pat, "/")+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
		if strings.Contains(pat, "/") {
			if ok, _ := path.Match(pat, rel); ok {
				return true
			}
			// Un patrón de carpeta (p. ej. "Fotos/RAW") excluye todo su contenido.
			if strings.HasPrefix(rel, strings.TrimSuffix(pat, "/")+"/") {
				return true
			}
		}
	}
	return false
}

// ExcludePattern devuelve el patrón de exclusión que casa exactamente con la
// ruta relativa rel y con nada más: anclado con "/" y con los metacaracteres
// de glob escapados.
func ExcludePattern(rel string) string {
	return "/" + globEscaper.Replace(rel)
}

var globEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)

// ExcludeTarget deshace ExcludePattern: si pat es un patrón anclado sin
// comodines devuelve la ruta relativa exacta a la que apunta y true. Un patrón
// sin anclar o con comodines sin escapar devuelve ("", false).
func ExcludeTarget(pat string) (string, bool) {
	if !strings.HasPrefix(pat, "/") {
		return "", false
	}
	var b strings.Builder
	s := pat[1:]
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '*' || c == '?' || c == '[':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// Excluded indica si el fichero rel quedaría fuera del recorrido por el
// patrón pat, con la misma regla que Scan: el patrón se prueba contra el
// propio fichero y contra cada carpeta ancestra (una carpeta excluida se
// lleva todo su contenido).
func Excluded(pat, rel string) bool {
	pats := []string{pat}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			dir := rel[:i]
			if matchesAny(pats, path.Base(dir), dir) {
				return true
			}
		}
	}
	return matchesAny(pats, path.Base(rel), rel)
}
