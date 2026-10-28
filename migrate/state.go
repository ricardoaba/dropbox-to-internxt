package migrate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Estados posibles de un fichero en el diario.
const (
	StatusDone    = "done"    // subido y verificado
	StatusFailed  = "failed"  // la última subida falló; se reintenta en el siguiente lote
	StatusSkipped = "skipped" // excluido de forma permanente (p. ej. supera el tamaño máximo)
)

// Record es una línea del diario: el resultado de procesar un fichero.
type Record struct {
	Time    time.Time `json:"ts"`
	Path    string    `json:"path"`             // ruta relativa al origen
	Status  string    `json:"status"`           // done | failed | skipped
	Size    int64     `json:"size"`             // tamaño del fichero local en el escaneo (0 si era un marcador)
	ModTime int64     `json:"mtime"`            // segundos Unix del fichero origen
	Bytes   int64     `json:"bytes,omitempty"`  // bytes realmente subidos (difiere de Size en los marcadores)
	Source  string    `json:"source,omitempty"` // "local" o "dropbox-api"
	SHA256  string    `json:"sha256,omitempty"`
	Remote  string    `json:"remote,omitempty"` // ruta en Internxt
	Error   string    `json:"error,omitempty"`
}

// UploadedBytes devuelve los bytes que ocupa el fichero en el destino.
func (r Record) UploadedBytes() int64 {
	if r.Bytes > 0 {
		return r.Bytes
	}
	return r.Size
}

// State es el diario de migración: un fichero JSONL de solo añadido donde
// cada línea es un Record. El estado actual de cada fichero es su última línea.
// Al ser de solo añadido, un corte de luz a mitad de escritura pierde como
// mucho la última línea, y esa subida se repetirá en el siguiente lote.
type State struct {
	mu      sync.Mutex
	path    string
	records map[string]Record
	streaks map[string]failStreak
	f       *os.File
}

// failStreak cuenta los fallos consecutivos de una ruta con el mismo error.
// Un registro con otro estado, o un fallo con otro error, reinicia la cuenta.
type failStreak struct {
	N   int
	Err string
}

func (s *State) track(r Record) {
	if r.Status != StatusFailed {
		delete(s.streaks, r.Path)
		return
	}
	fs := s.streaks[r.Path]
	if fs.N > 0 && fs.Err == r.Error {
		fs.N++
	} else {
		fs = failStreak{N: 1, Err: r.Error}
	}
	s.streaks[r.Path] = fs
}

// FailStreak devuelve cuántas veces seguidas ha fallado una ruta con el
// mismo error (0 si su último registro no es un fallo).
func (s *State) FailStreak(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streaks[path].N
}

// OpenState abre (o crea) el diario y reconstruye el estado en memoria.
func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s := &State{path: path, records: make(map[string]Record), streaks: make(map[string]failStreak), f: f}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			// Una última línea truncada por un corte es recuperable: se ignora.
			fmt.Fprintf(os.Stderr, "aviso: línea %d del diario ilegible, se ignora: %v\n", line, err)
			continue
		}
		s.records[r.Path] = r
		s.track(r)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, fmt.Errorf("leyendo %s: %w", path, err)
	}
	return s, nil
}

// Get devuelve el último registro de una ruta.
func (s *State) Get(path string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[path]
	return r, ok
}

// Append añade un registro al diario y lo fuerza a disco.
func (s *State) Append(r Record) error {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.records[r.Path] = r
	s.track(r)
	return nil
}

// All devuelve una copia de todos los registros actuales.
func (s *State) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	return out
}

// Len devuelve el número de rutas con registro.
func (s *State) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Close cierra el diario.
func (s *State) Close() error {
	if s.f == nil {
		return errors.New("diario ya cerrado")
	}
	err := s.f.Close()
	s.f = nil
	return err
}
