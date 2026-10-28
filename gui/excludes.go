package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"INTERNXT/migrate"
)

// ExcludeRow describe una exclusión configurada, con su efecto sobre la
// carpeta de Dropbox, para la ventana de gestión de exclusiones.
type ExcludeRow struct {
	Pattern string   `json:"pattern"`
	Target  string   `json:"target"`  // ruta exacta a la que apunta (vacío si es un patrón con comodines o sin anclar)
	Kind    string   `json:"kind"`    // fichero | carpeta | nombre | patrón
	Exists  bool     `json:"exists"`  // la ruta exacta existe hoy en la carpeta de Dropbox
	Matches int      `json:"matches"` // ficheros del origen que deja fuera
	Bytes   int64    `json:"bytes"`   // tamaño en disco de esos ficheros (los marcadores pesan 0)
	Samples []string `json:"samples"` // hasta sampleLimit rutas afectadas
	// Último registro del diario para la ruta exacta (por qué se saltó).
	Status string `json:"status"`
	Error  string `json:"error"`
	Time   string `json:"time"`
}

// ExcludeReport es el estado completo de las exclusiones.
type ExcludeReport struct {
	Rows          []ExcludeRow `json:"rows"`
	Defaults      []string     `json:"defaults"`      // exclusiones integradas, siempre activas
	Files         int          `json:"files"`         // ficheros del origen sin aplicar las exclusiones del usuario
	Excluded      int          `json:"excluded"`      // ficheros que alguna exclusión del usuario deja fuera
	ExcludedBytes int64        `json:"excludedBytes"` //
}

const sampleLimit = 5

// unfilteredScan recorre el origen aplicando solo las exclusiones integradas,
// para poder medir cuántos ficheros deja fuera cada exclusión del usuario.
// Se guarda en caché unos minutos: "Probar" se llama varias veces seguidas.
func (a *App) unfilteredScan(cfg Config) ([]migrate.File, error) {
	a.mu.Lock()
	if a.allFiles != nil && a.allFilesDir == cfg.SourceDir && time.Since(a.allFilesAt) < 5*time.Minute {
		files := a.allFiles
		a.mu.Unlock()
		return files, nil
	}
	a.mu.Unlock()
	files, err := migrate.Scan(migrate.ScanOptions{SourceDir: cfg.SourceDir})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.allFiles, a.allFilesDir, a.allFilesAt = files, cfg.SourceDir, time.Now()
	a.mu.Unlock()
	return files, nil
}

func excludeRow(cfg Config, st *migrate.State, pat string, files []migrate.File) ExcludeRow {
	row := ExcludeRow{Pattern: pat, Kind: "patrón", Samples: []string{}}
	if target, ok := migrate.ExcludeTarget(pat); ok {
		row.Target = target
		row.Kind = "fichero"
		if fi, err := os.Stat(filepath.Join(cfg.SourceDir, filepath.FromSlash(target))); err == nil {
			row.Exists = true
			if fi.IsDir() {
				row.Kind = "carpeta"
			}
		}
		if st != nil {
			if r, ok := st.Get(target); ok {
				row.Status, row.Error, row.Time = r.Status, r.Error, r.Time.Format("02/01/06 15:04")
			}
		}
	} else if !strings.ContainsAny(pat, "*?[") {
		row.Kind = "nombre"
	}
	for _, f := range files {
		if !migrate.Excluded(pat, f.Rel) {
			continue
		}
		row.Matches++
		row.Bytes += f.Size
		if len(row.Samples) < sampleLimit {
			row.Samples = append(row.Samples, f.Rel)
		}
	}
	return row
}

// ExcludeReport devuelve todas las exclusiones del usuario con su efecto
// sobre la carpeta de Dropbox y, para las rutas exactas, el motivo por el que
// se saltaron según el diario.
func (a *App) ExcludeReport() (ExcludeReport, error) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	if !dirExists(cfg.SourceDir) {
		return ExcludeReport{}, errors.New("la carpeta de Dropbox no existe: " + cfg.SourceDir)
	}
	files, err := a.unfilteredScan(cfg)
	if err != nil {
		return ExcludeReport{}, err
	}
	a.mu.Lock()
	st, _ := a.openState(cfg.StatePath)
	a.mu.Unlock()

	rep := ExcludeReport{Rows: make([]ExcludeRow, 0, len(cfg.Excludes)), Defaults: migrate.DefaultExcludes, Files: len(files)}
	hit := make([]bool, len(files))
	for _, pat := range cfg.Excludes {
		rep.Rows = append(rep.Rows, excludeRow(cfg, st, pat, files))
		for i, f := range files {
			if !hit[i] && migrate.Excluded(pat, f.Rel) {
				hit[i] = true
				rep.Excluded++
				rep.ExcludedBytes += f.Size
			}
		}
	}
	return rep, nil
}

// TestExclude mide el efecto que tendría un patrón nuevo sin guardarlo.
func (a *App) TestExclude(pat string) (ExcludeRow, error) {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return ExcludeRow{}, errors.New("patrón vacío")
	}
	if _, err := filepath.Match(strings.TrimPrefix(pat, "/"), ""); err != nil {
		return ExcludeRow{}, errors.New("patrón mal formado: " + err.Error())
	}
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	files, err := a.unfilteredScan(cfg)
	if err != nil {
		return ExcludeRow{}, err
	}
	a.mu.Lock()
	st, _ := a.openState(cfg.StatePath)
	a.mu.Unlock()
	return excludeRow(cfg, st, pat, files), nil
}

// SaveExcludes sustituye la lista de exclusiones (sin tocar el resto de la
// configuración), la guarda y devuelve la configuración resultante. Los
// patrones vacíos y repetidos se descartan; el orden se conserva.
func (a *App) SaveExcludes(patterns []string) (Config, error) {
	clean := make([]string, 0, len(patterns))
	seen := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if _, err := filepath.Match(strings.TrimPrefix(p, "/"), ""); err != nil {
			return a.GetConfig(), errors.New("patrón mal formado: " + p)
		}
		seen[p] = true
		clean = append(clean, p)
	}
	a.mu.Lock()
	cfg := a.cfg
	before := len(cfg.Excludes)
	cfg.Excludes = clean
	a.mu.Unlock()
	if err := a.persistConfig(cfg); err != nil {
		return a.GetConfig(), err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.plan = nil
	a.mu.Unlock()
	a.log("exclusiones guardadas: %d (antes %d)", len(clean), before)
	return cfg, nil
}
