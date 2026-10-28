package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"INTERNXT/dropbox"
	"INTERNXT/migrate"
	"INTERNXT/webdav"
)

// Config es la configuración persistente de la aplicación.
// Se guarda en ~/Library/Application Support/Dropbox a Internxt/config.json.
type Config struct {
	SourceDir       string   `json:"sourceDir"`
	RemoteRoot      string   `json:"remoteRoot"`
	StatePath       string   `json:"statePath"`
	WebDAVURL       string   `json:"webdavUrl"`
	WebDAVUser      string   `json:"webdavUser"`
	WebDAVPass      string   `json:"webdavPass"`
	Insecure        bool     `json:"insecure"`
	BatchFiles      int      `json:"batchFiles"`
	BatchMB         int64    `json:"batchMB"`
	Verify          string   `json:"verify"`
	Parallel        int      `json:"parallel"`
	Retries         int      `json:"retries"`
	QuarantineAfter int      `json:"quarantineAfter"` // aparcar tras N fallos seguidos con el mismo error (0 = nunca)
	MaxFileGB       int64    `json:"maxFileGB"`
	Excludes        []string `json:"excludes"`
	ComposeDir      string   `json:"composeDir"`
	ScheduleMinutes int      `json:"scheduleMinutes"`
	InxtUser        string   `json:"inxtUser"`     // cuenta Internxt para el contenedor
	InxtPassword    string   `json:"inxtPassword"` // contraseña de la cuenta Internxt
	InxtOTPToken    string   `json:"inxtOtpToken"` // secreto TOTP (opcional)

	// Ficheros "solo online" de Dropbox: se descargan de la nube por la API.
	FetchOnlineOnly     bool   `json:"fetchOnlineOnly"`
	DropboxAppKey       string `json:"dropboxAppKey"`
	DropboxRefreshToken string `json:"dropboxRefreshToken"`
	// DropboxAuthError guarda el último fallo de autorización (token revocado)
	// para avisar en la interfaz hasta que se vuelva a conectar.
	DropboxAuthError string `json:"dropboxAuthError,omitempty"`
	DropboxAccount   string `json:"dropboxAccount"` // email mostrado tras conectar
}

// FileRow es una fila de fichero para la interfaz.
type FileRow struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Summary es el resultado de un escaneo para la interfaz.
type Summary struct {
	TotalFiles   int       `json:"totalFiles"`
	TotalBytes   int64     `json:"totalBytes"`
	Done         int       `json:"done"`
	DoneBytes    int64     `json:"doneBytes"`
	Pending      int       `json:"pending"`
	PendingBytes int64     `json:"pendingBytes"`
	Retries      int       `json:"retries"`
	Changed      int       `json:"changed"`
	Skipped      int       `json:"skipped"`
	SkippedBytes int64     `json:"skippedBytes"`
	TooLarge     []FileRow `json:"tooLarge"`
	Percent      float64   `json:"percent"`
	ScannedAt    string    `json:"scannedAt"`
	// Marcadores "solo online": en cola (se descargarán) o fuera de ella.
	Placeholders        int `json:"placeholders"`
	PlaceholdersSkipped int `json:"placeholdersSkipped"`
	EmptyUploads        int `json:"emptyUploads"`
	// Quarantined son los ficheros aparcados: fallaron QuarantineAfter veces
	// seguidas con el mismo error y no entran en la cola hasta que se pulse
	// "Reintentar aparcados".
	Quarantined      []FailedRow `json:"quarantined"`
	QuarantinedBytes int64       `json:"quarantinedBytes"`
}

// ProgressEvent es el avance de un fichero en curso.
type ProgressEvent struct {
	Path  string `json:"path"`
	Phase string `json:"phase"` // download | wait | upload | verify
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// FailedRow es un fichero con fallo registrado en el diario.
type FailedRow struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Error string `json:"error"`
	Time  string `json:"time"`
}

// FileEvent se emite por cada fichero procesado en un lote.
type FileEvent struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Bytes   int64  `json:"bytes"` // bytes realmente subidos (los marcadores tienen Size 0)
	Status  string `json:"status"`
	Error   string `json:"error"`
	Millis  int64  `json:"millis"`
	Attempt int    `json:"attempt"`
	Index   int    `json:"index"`
	Total   int    `json:"total"`
}

// BatchResult se emite al terminar un lote.
type BatchResult struct {
	Done      int     `json:"done"`
	Failed    int     `json:"failed"`
	Bytes     int64   `json:"bytes"`
	Seconds   float64 `json:"seconds"`
	Remaining int     `json:"remaining"`
	Stopped   bool    `json:"stopped"`
	Error     string  `json:"error"`
	// Continuous indica que el lote forma parte de "Migrar todo" y, si quedan
	// pendientes y no hubo error, vendrá otro a continuación.
	Continuous bool `json:"continuous"`
	// AuthLost indica que el error es una autorización de Dropbox revocada:
	// la interfaz pide volver a conectar la cuenta.
	AuthLost bool `json:"authLost"`
}

// App es el backend de la interfaz. Sus métodos exportados quedan
// disponibles en el frontend a través de los bindings de Wails.
type App struct {
	ctx     context.Context
	cfgPath string

	mu        sync.Mutex
	cfg       Config
	state     *migrate.State
	statePath string
	plan      *migrate.Plan
	busy      string // "" | "batch" | "verify"
	cancel    context.CancelFunc
	stopAfter bool              // "Migrar todo": no encadenar otro lote cuando acabe el actual
	mig       *migrate.Migrator // migrador del lote en curso (para Skip), o nil
	schedStop chan struct{}

	// Último recorrido del origen sin las exclusiones del usuario (ventana de exclusiones).
	allFiles    []migrate.File
	allFilesDir string
	allFilesAt  time.Time
}

// NewApp crea la aplicación.
func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.cfgPath = filepath.Join(appSupportDir(), "config.json")
	if p := os.Getenv("DROPBOX2INTERNXT_CONFIG"); p != "" { // pruebas: configuración alternativa
		a.cfgPath = p
	}
	a.cfg = a.loadConfig()
	a.applySchedule()
	a.cleanOrphanTemps()
}

// cleanOrphanTemps borra temporales de descargas de Dropbox que quedaron de
// una ejecución anterior interrumpida (cierre forzoso de la app). Solo hay
// una instancia de la app, así que ninguno puede estar en uso.
func (a *App) cleanOrphanTemps() {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "dropbox2internxt-*"))
	var n int
	var bytes int64
	for _, p := range matches {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			bytes += st.Size()
			if os.Remove(p) == nil {
				n++
			}
		}
	}
	if n > 0 {
		a.log("borrados %d temporales huérfanos de descargas anteriores (%s)", n, humanBytes(bytes))
	}
}

func (a *App) shutdown(_ context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	if a.schedStop != nil {
		close(a.schedStop)
		a.schedStop = nil
	}
	if a.state != nil {
		a.state.Close()
	}
}

// ---------- configuración ----------

func appSupportDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Library", "Application Support", "Dropbox a Internxt")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	src := filepath.Join(home, "Dropbox")
	if alt := filepath.Join(home, "Library", "CloudStorage", "Dropbox"); !dirExists(src) && dirExists(alt) {
		src = alt
	}
	return Config{
		SourceDir:       src,
		RemoteRoot:      "/Dropbox",
		StatePath:       filepath.Join(appSupportDir(), "diario.jsonl"),
		WebDAVURL:       "http://127.0.0.1:3005",
		WebDAVUser:      "webdav",
		BatchFiles:      200,
		BatchMB:         2048,
		Verify:          migrate.VerifyHash,
		Parallel:        2,
		Retries:         3,
		QuarantineAfter: 3,
		MaxFileGB:       100,
		Excludes:        []string{},
		ComposeDir:      findComposeDir(),
	}
}

func (a *App) loadConfig() Config {
	cfg := defaultConfig()
	b, err := os.ReadFile(a.cfgPath)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		a.log("aviso: config.json ilegible, se usan valores por defecto: %v", err)
		return defaultConfig()
	}
	if cfg.Excludes == nil {
		cfg.Excludes = []string{}
	}
	return cfg
}

// GetConfig devuelve la configuración actual.
func (a *App) GetConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SaveConfig valida y guarda la configuración.
func (a *App) SaveConfig(cfg Config) error {
	if strings.TrimSpace(cfg.SourceDir) == "" || !dirExists(cfg.SourceDir) {
		return fmt.Errorf("la carpeta de origen no existe: %s", cfg.SourceDir)
	}
	if !strings.HasPrefix(cfg.RemoteRoot, "/") {
		cfg.RemoteRoot = "/" + cfg.RemoteRoot
	}
	if cfg.Verify != migrate.VerifySize && cfg.Verify != migrate.VerifyHash {
		return errors.New("la verificación debe ser 'size' o 'hash'")
	}
	if cfg.BatchFiles < 0 || cfg.BatchMB < 0 || cfg.Parallel < 1 || cfg.Retries < 0 || cfg.QuarantineAfter < 0 || cfg.MaxFileGB < 0 || cfg.ScheduleMinutes < 0 {
		return errors.New("hay valores numéricos fuera de rango")
	}
	if cfg.Parallel > 8 {
		cfg.Parallel = 8
	}
	if strings.TrimSpace(cfg.StatePath) == "" {
		cfg.StatePath = filepath.Join(appSupportDir(), "diario.jsonl")
	}
	if cfg.Excludes == nil {
		cfg.Excludes = []string{}
	}
	if _, err := webdav.New(cfg.WebDAVURL); err != nil {
		return err
	}

	a.mu.Lock()
	keyChanged := cfg.DropboxRefreshToken != "" && strings.TrimSpace(cfg.DropboxAppKey) != a.cfg.DropboxAppKey
	a.mu.Unlock()
	if keyChanged {
		// El token de refresco está ligado a la App key con la que se obtuvo:
		// con otra key Dropbox responde invalid_grant. Se olvida y se pide reconectar.
		cfg.DropboxRefreshToken = ""
		cfg.DropboxAccount = ""
		cfg.DropboxAuthError = ""
		cfg.FetchOnlineOnly = false
		a.log("la App key de Dropbox ha cambiado: hay que volver a conectar la cuenta")
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(a.cfgPath, b, 0o600); err != nil {
		return err
	}

	a.mu.Lock()
	a.cfg = cfg
	a.plan = nil
	a.mu.Unlock()
	a.applySchedule()
	a.log("configuración guardada")
	return nil
}

// ChooseSourceDir abre un diálogo para elegir la carpeta de Dropbox.
func (a *App) ChooseSourceDir() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Carpeta local de Dropbox",
		DefaultDirectory: a.GetConfig().SourceDir,
	})
}

// ChooseComposeDir abre un diálogo para elegir la carpeta con docker-compose.yml.
func (a *App) ChooseComposeDir() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Carpeta con docker-compose.yml del WebDAV",
		DefaultDirectory: a.GetConfig().ComposeDir,
	})
}

// ---------- conexión y Docker ----------

func (a *App) client() (*webdav.Client, error) {
	cfg := a.GetConfig()
	opts := []webdav.Option{webdav.WithTimeout(2 * time.Hour)}
	if cfg.WebDAVUser != "" {
		opts = append(opts, webdav.WithBasicAuth(cfg.WebDAVUser, cfg.WebDAVPass))
	}
	if cfg.Insecure {
		opts = append(opts, webdav.WithInsecureTLS())
	}
	return webdav.New(cfg.WebDAVURL, opts...)
}

// TestConnection comprueba que el WebDAV responde y devuelve un resumen.
func (a *App) TestConnection() (string, error) {
	c, err := a.client()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	entries, err := c.List(ctx, "/")
	if err != nil {
		return "", err
	}
	dirs, files := 0, 0
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	return fmt.Sprintf("Conectado. Raíz del Drive: %d carpetas y %d ficheros.", dirs, files), nil
}

func (a *App) dockerCompose(args ...string) (string, error) {
	cfg := a.GetConfig()
	if cfg.ComposeDir == "" || !fileExists(filepath.Join(cfg.ComposeDir, "docker-compose.yml")) {
		return "", errors.New("no se encuentra docker-compose.yml: indica la carpeta en Configuración")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	cmd.Dir = cfg.ComposeDir
	cmd.Env = dockerEnv()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("docker compose %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

// DockerStatus indica si el contenedor del WebDAV está en marcha.
func (a *App) DockerStatus() (string, error) {
	out, err := a.dockerCompose("ps", "--status", "running", "--services")
	if err != nil {
		return "", err
	}
	if strings.Contains(out, "internxt-webdav") {
		return "en marcha", nil
	}
	return "parado", nil
}

// dockerEnv devuelve el entorno para invocar docker desde la app: las apps
// de macOS arrancan con un PATH mínimo, y con Colima el socket no está en
// /var/run/docker.sock sino en ~/.colima/<perfil>/docker.sock.
func dockerEnv() []string {
	env := append(os.Environ(), "PATH="+os.Getenv("PATH")+":/usr/local/bin:/opt/homebrew/bin")
	if os.Getenv("DOCKER_HOST") == "" && !fileExists("/var/run/docker.sock") {
		home, _ := os.UserHomeDir()
		for _, sock := range []string{
			filepath.Join(home, ".colima", "default", "docker.sock"),
			filepath.Join(home, ".colima", "docker.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".rd", "docker.sock"),
		} {
			if st, err := os.Stat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
				env = append(env, "DOCKER_HOST=unix://"+sock)
				break
			}
		}
	}
	return env
}

func lookPath(name string) (string, bool) {
	for _, dir := range append(filepath.SplitList(os.Getenv("PATH")), "/usr/local/bin", "/opt/homebrew/bin") {
		p := filepath.Join(dir, name)
		if fileExists(p) {
			return p, true
		}
	}
	return "", false
}

// DockerDaemonRunning indica si el demonio de Docker responde.
func (a *App) DockerDaemonRunning() bool {
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Env = dockerEnv()
	return cmd.Run() == nil
}

// startDockerRuntime arranca el motor de Docker disponible: Docker Desktop,
// OrbStack o Colima (Homebrew), y espera a que el demonio responda.
func (a *App) startDockerRuntime(step func(string)) error {
	switch {
	case dirExists("/Applications/Docker.app"):
		step("abriendo Docker Desktop…")
		if err := exec.Command("open", "-a", "Docker").Run(); err != nil {
			return fmt.Errorf("no se pudo abrir Docker Desktop: %w", err)
		}
	case dirExists("/Applications/OrbStack.app"):
		step("abriendo OrbStack…")
		if err := exec.Command("open", "-a", "OrbStack").Run(); err != nil {
			return fmt.Errorf("no se pudo abrir OrbStack: %w", err)
		}
	default:
		colima, ok := lookPath("colima")
		if !ok {
			return errors.New("no se encuentra Docker Desktop, OrbStack ni Colima; arranca Docker a mano y vuelve a intentarlo")
		}
		step("arrancando Colima (la máquina virtual tarda alrededor de un minuto)…")
		ctx, cancel := context.WithTimeout(a.ctx, 4*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, colima, "start")
		cmd.Env = dockerEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("colima start: %s", strings.TrimSpace(string(out)))
		}
	}

	deadline := time.Now().Add(2 * time.Minute)
	for !a.DockerDaemonRunning() {
		if time.Now().After(deadline) {
			return errors.New("Docker no ha respondido en 2 minutos; arráncalo a mano y vuelve a intentarlo")
		}
		time.Sleep(3 * time.Second)
		step("esperando a Docker…")
	}
	step("Docker en marcha")
	return nil
}

// writeEnvFile escribe (o actualiza) el .env que lee docker-compose con las
// credenciales de Internxt y la configuración del WebDAV. Conserva las
// variables que ya existieran y no gestiona la app.
func (a *App) writeEnvFile(cfg Config) error {
	path := filepath.Join(cfg.ComposeDir, ".env")
	vars := map[string]string{}
	var order []string
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			k, v, ok := strings.Cut(t, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			if _, seen := vars[k]; !seen {
				order = append(order, k)
			}
			vars[k] = strings.TrimSpace(v)
		}
	}

	u, err := url.Parse(cfg.WebDAVURL)
	if err != nil {
		return err
	}
	port := u.Port()
	if port == "" {
		port = "3005"
	}
	set := func(k, v string) {
		if _, seen := vars[k]; !seen {
			order = append(order, k)
		}
		vars[k] = v
	}
	set("INXT_USER", cfg.InxtUser)
	set("INXT_PASSWORD", cfg.InxtPassword)
	set("INXT_OTPTOKEN", cfg.InxtOTPToken)
	set("WEBDAV_PORT", port)
	set("WEBDAV_PROTOCOL", u.Scheme)
	if cfg.WebDAVUser != "" {
		set("WEBDAV_CUSTOM_AUTH", "true")
		set("WEBDAV_USERNAME", cfg.WebDAVUser)
		set("WEBDAV_PASSWORD", cfg.WebDAVPass)
	} else {
		set("WEBDAV_CUSTOM_AUTH", "false")
	}
	set("INXT_WEBDAV_URL", cfg.WebDAVURL)
	set("INXT_WEBDAV_USER", cfg.WebDAVUser)
	set("INXT_WEBDAV_PASS", cfg.WebDAVPass)

	var sb strings.Builder
	sb.WriteString("# Generado por la app 'Dropbox a Internxt'. Contiene credenciales: no lo subas a git.\n")
	for _, k := range order {
		fmt.Fprintf(&sb, "%s=%s\n", k, vars[k])
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// DockerUp prepara el .env, arranca Docker Desktop si hace falta, levanta el
// contenedor y espera a que el WebDAV responda. Emite "docker:step" con el progreso.
func (a *App) DockerUp() (string, error) {
	cfg := a.GetConfig()
	if cfg.ComposeDir == "" || !fileExists(filepath.Join(cfg.ComposeDir, "docker-compose.yml")) {
		return "", errors.New("no se encuentra docker-compose.yml: indica la carpeta en Configuración → Docker")
	}
	if strings.TrimSpace(cfg.InxtUser) == "" || cfg.InxtPassword == "" {
		return "", errors.New("faltan el email y la contraseña de la cuenta Internxt en Configuración → Cuenta Internxt")
	}
	if cfg.WebDAVUser != "" && cfg.WebDAVPass == "" {
		return "", errors.New("el WebDAV tiene usuario pero no contraseña: rellena ambos o deja el usuario vacío")
	}

	step := func(msg string) {
		a.log("%s", msg)
		a.emit("docker:step", msg)
	}

	step("escribiendo .env en " + cfg.ComposeDir)
	if err := a.writeEnvFile(cfg); err != nil {
		return "", fmt.Errorf("escribiendo .env: %w", err)
	}

	if !a.DockerDaemonRunning() {
		step("Docker no está en marcha")
		if err := a.startDockerRuntime(step); err != nil {
			return "", err
		}
	}

	step("docker compose up -d (la primera vez descarga la imagen, puede tardar)")
	out, err := a.dockerCompose("up", "-d")
	if err != nil {
		return out, err
	}

	step("contenedor arrancado; esperando al login en Internxt y al WebDAV…")
	client, err := a.client()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
		_, lerr := client.List(ctx, "/")
		cancel()
		if lerr == nil {
			step("WebDAV en línea")
			return "WebDAV en línea", nil
		}
		if time.Now().After(deadline) {
			logs, _ := a.dockerCompose("logs", "--tail", "15", "--no-color")
			return logs, fmt.Errorf("el contenedor está arrancado pero el WebDAV no responde (%v). Últimas líneas del log:\n%s", lerr, logs)
		}
		time.Sleep(3 * time.Second)
	}
}

// DockerDown para el contenedor del WebDAV.
func (a *App) DockerDown() (string, error) {
	a.log("docker compose down ...")
	return a.dockerCompose("down")
}

// DockerLogs devuelve las últimas líneas del log del contenedor.
func (a *App) DockerLogs() (string, error) {
	return a.dockerCompose("logs", "--tail", "40", "--no-color")
}

// ---------- escaneo y plan ----------

func (a *App) openState(path string) (*migrate.State, error) {
	if a.state != nil && a.statePath == path {
		return a.state, nil
	}
	if a.state != nil {
		a.state.Close()
		a.state = nil
	}
	st, err := migrate.OpenState(path)
	if err != nil {
		return nil, err
	}
	a.state = st
	a.statePath = path
	return st, nil
}

// Scan recorre el origen, lo compara con el diario y devuelve el resumen.
func (a *App) Scan() (Summary, error) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()

	files, err := migrate.Scan(migrate.ScanOptions{SourceDir: cfg.SourceDir, Excludes: cfg.Excludes})
	if err != nil {
		return Summary{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.openState(cfg.StatePath)
	if err != nil {
		return Summary{}, err
	}
	plan := migrate.BuildPlan(files, st, migrate.PlanOptions{
		MaxFileSize:      cfg.MaxFileGB << 30,
		SkipPlaceholders: !cfg.FetchOnlineOnly,
		QuarantineAfter:  cfg.QuarantineAfter,
	})
	a.plan = &plan
	s := summaryOf(plan, st)
	go a.emit("summary", s)
	return s, nil
}

func summaryOf(p migrate.Plan, st *migrate.State) Summary {
	s := Summary{
		TotalFiles: p.TotalFiles, TotalBytes: p.TotalBytes,
		Done: p.Done, DoneBytes: p.DoneBytes,
		Pending: len(p.Pending), PendingBytes: p.PendingBytes(),
		Retries: p.Retries, Changed: p.Changed,
		Skipped: p.Skipped, SkippedBytes: p.SkippedBytes,
		TooLarge:            make([]FileRow, 0, len(p.TooLarge)),
		ScannedAt:           time.Now().Format("15:04:05"),
		Placeholders:        p.Placeholders,
		PlaceholdersSkipped: p.PlaceholdersSkipped,
		EmptyUploads:        p.EmptyUploads,
		Quarantined:         make([]FailedRow, 0, len(p.Quarantined)),
		QuarantinedBytes:    p.QuarantinedBytes,
	}
	for _, f := range p.TooLarge {
		s.TooLarge = append(s.TooLarge, FileRow{Path: f.Rel, Size: f.Size})
	}
	for _, f := range p.Quarantined {
		row := FailedRow{Path: f.Rel, Size: f.Size}
		if r, ok := st.Get(f.Rel); ok {
			row.Error, row.Time = r.Error, r.Time.Format("02/01 15:04")
		}
		s.Quarantined = append(s.Quarantined, row)
	}
	if p.TotalBytes > 0 {
		s.Percent = float64(p.DoneBytes) / float64(p.TotalBytes) * 100
	}
	return s
}

// PreviewBatch devuelve el siguiente lote sin subir nada.
func (a *App) PreviewBatch() ([]FileRow, error) {
	a.mu.Lock()
	plan := a.plan
	cfg := a.cfg
	a.mu.Unlock()
	if plan == nil {
		if _, err := a.Scan(); err != nil {
			return nil, err
		}
		a.mu.Lock()
		plan = a.plan
		a.mu.Unlock()
	}
	batch := plan.Batch(cfg.BatchFiles, cfg.BatchMB<<20)
	rows := make([]FileRow, 0, len(batch))
	for _, f := range batch {
		rows = append(rows, FileRow{Path: f.Rel, Size: f.Size})
	}
	return rows, nil
}

// ListFailed devuelve los ficheros cuyo último registro es un fallo.
func (a *App) ListFailed() ([]FailedRow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.openState(a.cfg.StatePath)
	if err != nil {
		return nil, err
	}
	var rows []FailedRow
	for _, r := range st.All() {
		if r.Status == migrate.StatusFailed {
			rows = append(rows, FailedRow{Path: r.Path, Size: r.Size, Error: r.Error, Time: r.Time.Format("02/01 15:04")})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	if rows == nil {
		rows = []FailedRow{}
	}
	return rows, nil
}

// IsBusy indica si hay un lote o una verificación en curso.
func (a *App) IsBusy() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}

// StopAfterBatch alterna "parar al acabar el lote" durante "Migrar todo":
// el lote en curso se termina entero y no se encadena el siguiente, de modo
// que se puede cerrar la app o reiniciar el Mac sin perder trabajo a medias.
// Devuelve el estado resultante.
func (a *App) StopAfterBatch() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy != "all" {
		a.stopAfter = false
		return false
	}
	a.stopAfter = !a.stopAfter
	if a.stopAfter {
		a.log("se parará al acabar el lote en curso; no se encadenará otro")
	} else {
		a.log("cancelado: se seguirá encadenando lotes")
	}
	return a.stopAfter
}

// stopAfterRequested indica si se pidió parar al acabar el lote.
func (a *App) stopAfterRequested() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopAfter
}

// SkipFile deja de migrar un fichero: lo añade a las exclusiones como patrón
// exacto (anclado y con los comodines escapados) y lo guarda, y si hay un
// lote en curso lo saca de él: si está en cola no se toca y si está bajando
// o subiendo se interrumpe. Devuelve la configuración resultante.
func (a *App) SkipFile(rel string) (Config, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return a.GetConfig(), errors.New("ruta vacía")
	}
	pat := migrate.ExcludePattern(rel)
	a.mu.Lock()
	cfg := a.cfg
	already := false
	for _, e := range cfg.Excludes {
		if e == pat {
			already = true
			break
		}
	}
	if !already {
		cfg.Excludes = append(append([]string{}, cfg.Excludes...), pat)
	}
	mig := a.mig
	a.mu.Unlock()
	if !already {
		if err := a.persistConfig(cfg); err != nil {
			return a.GetConfig(), err
		}
		a.mu.Lock()
		a.cfg = cfg
		a.plan = nil
		a.mu.Unlock()
	}
	if mig != nil {
		mig.Skip(rel)
		a.log("saltado: %s (excluido de los próximos lotes y retirado del lote en curso)", rel)
	} else {
		a.log("saltado: %s (excluido de los próximos lotes)", rel)
	}
	return cfg, nil
}

// Stop cancela la operación en curso. Lo ya completado queda anotado.
func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.log("deteniendo... se termina el fichero en curso")
		a.cancel()
	}
}

// ---------- lote ----------

// StartBatch migra el siguiente lote en segundo plano. Emite los eventos
// "batch:start", "file", "batch:end" y "summary".
func (a *App) StartBatch() error {
	return a.startBatch("manual", false, false)
}

// RetryQuarantined lanza un lote formado solo por los ficheros aparcados.
// Si vuelven a fallar, quedan aparcados otra vez (la racha sigue).
func (a *App) RetryQuarantined() error {
	return a.startBatch("aparcados", false, true)
}

// StartAll encadena lotes hasta que no quede nada pendiente, se pulse Detener
// o un lote termine con error o sin ningún fichero correcto. Mientras dura,
// evita que el Mac entre en reposo (caffeinate).
func (a *App) StartAll() error {
	return a.startBatch("continuo", true, false)
}

// startBatch ejecuta un lote (o una cadena de ellos con loop). Con
// quarantined, el lote son solo los ficheros aparcados.
func (a *App) startBatch(origin string, loop, quarantined bool) error {
	a.mu.Lock()
	if a.busy != "" {
		a.mu.Unlock()
		return fmt.Errorf("ya hay una operación en curso (%s)", a.busy)
	}
	cfg := a.cfg
	ctx, cancel := context.WithCancel(a.ctx)
	a.busy = "batch"
	if loop {
		a.busy = "all"
	}
	a.cancel = cancel
	a.stopAfter = false
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.busy = ""
			a.cancel = nil
			a.stopAfter = false
			a.mu.Unlock()
			cancel()
			if _, err := a.Scan(); err != nil {
				a.log("error al reescanear: %v", err)
			}
		}()
		if loop {
			if caff := exec.CommandContext(ctx, "caffeinate", "-i"); caff.Start() == nil {
				defer func() { _ = caff.Process.Kill(); _ = caff.Wait() }()
				a.log("migración continua: el Mac no entrará en reposo mientras dure")
			}
		}
		batches := 0
		for {
			batches++
			res := a.runBatch(ctx, cfg, origin, quarantined)
			stopAfter := loop && a.stopAfterRequested()
			res.Continuous = loop && !stopAfter
			a.emit("batch:end", res)
			if stopAfter {
				a.log("lote terminado; parada pedida al acabar el lote: no se encadena otro")
				break
			}
			if !loop || res.Error != "" || ctx.Err() != nil || res.Remaining <= 0 || res.Done == 0 {
				break
			}
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
			}
		}
		if loop {
			a.log("migración continua terminada tras %d lotes", batches)
			a.emit("all:end", map[string]any{"batches": batches, "stopped": ctx.Err() != nil})
		}
	}()
	return nil
}

func (a *App) runBatch(ctx context.Context, cfg Config, origin string, quarantined bool) BatchResult {
	a.log("lote %s: escaneando %s", origin, cfg.SourceDir)
	if _, err := a.Scan(); err != nil {
		return BatchResult{Error: err.Error()}
	}
	a.mu.Lock()
	plan := *a.plan
	a.mu.Unlock()

	client, err := a.client()
	if err != nil {
		return BatchResult{Error: err.Error()}
	}
	if _, err := client.List(ctx, "/"); err != nil {
		return BatchResult{Error: "el WebDAV no responde: " + err.Error()}
	}

	a.mu.Lock()
	st, err := a.openState(cfg.StatePath)
	a.mu.Unlock()
	if err != nil {
		return BatchResult{Error: err.Error()}
	}

	var fetcher migrate.Fetcher
	if cfg.FetchOnlineOnly {
		if cfg.DropboxAppKey == "" || cfg.DropboxRefreshToken == "" {
			return BatchResult{Error: "para descargar los ficheros \"solo online\" conecta la cuenta de Dropbox en Configuración"}
		}
		fetcher = &dropbox.Fetcher{Client: dropbox.New(cfg.DropboxAppKey, cfg.DropboxRefreshToken)}
	}

	var (
		progMu   sync.Mutex
		progLast = map[string]time.Time{}
	)
	m := migrate.New(client, st, migrate.Options{
		RemoteRoot: cfg.RemoteRoot, Verify: cfg.Verify, Parallel: cfg.Parallel, Retries: cfg.Retries,
		Fetcher: fetcher, MaxFileSize: cfg.MaxFileGB << 30,
		Progress: func(f migrate.File, phase string, done, total int64) {
			key := f.Rel + "|" + phase
			progMu.Lock()
			last := progLast[key]
			final := total > 0 && done >= total
			if !final && time.Since(last) < 300*time.Millisecond {
				progMu.Unlock()
				return
			}
			progLast[key] = time.Now()
			progMu.Unlock()
			a.emit("file:progress", ProgressEvent{Path: f.Rel, Phase: phase, Done: done, Total: total})
		},
	})
	a.mu.Lock()
	a.mig = m
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.mig = nil
		a.mu.Unlock()
	}()
	if len(plan.TooLarge) > 0 {
		if err := m.MarkSkipped(plan.TooLarge, fmt.Sprintf("supera el máximo de %d GB", cfg.MaxFileGB)); err != nil {
			return BatchResult{Error: err.Error()}
		}
	}

	batch := plan.Batch(cfg.BatchFiles, cfg.BatchMB<<20)
	if quarantined {
		batch = plan.Quarantined
		if len(batch) == 0 {
			a.log("no hay ficheros aparcados")
			return BatchResult{Remaining: len(plan.Pending)}
		}
		a.log("reintentando %d ficheros aparcados", len(batch))
	}
	if len(batch) == 0 {
		a.log("nada pendiente: migración completa")
		return BatchResult{}
	}
	var batchBytes int64
	rows := make([]FileRow, 0, len(batch))
	for _, f := range batch {
		batchBytes += f.Size
		rows = append(rows, FileRow{Path: f.Rel, Size: f.Size})
	}
	// Comprobación previa: si el lote incluye marcadores "solo online", la
	// autorización de Dropbox tiene que seguir válida antes de tocar nada;
	// así un token revocado produce un aviso claro y no un fallo por fichero.
	if sample := firstPlaceholder(batch); fetcher != nil && sample != "" {
		if res, bad := a.checkDropbox(ctx, fetcher.(*dropbox.Fetcher), sample); bad {
			return res
		}
	}

	// El lote lleva su propia lista de ficheros: para los aparcados no coincide
	// con la vista previa (PreviewBatch), que solo enseña los pendientes.
	a.emit("batch:start", map[string]any{"count": len(batch), "bytes": batchBytes, "files": rows})
	a.log("lote de %d ficheros (%s), verificación por %s", len(batch), humanBytes(batchBytes), cfg.Verify)

	index := 0
	var imu sync.Mutex
	sum := m.Run(ctx, batch, func(ev migrate.Event) {
		imu.Lock()
		if ev.Status != migrate.StatusDeferred { // un aplazado no cuenta: volverá
			index++
		}
		i := index
		imu.Unlock()
		if ev.Status == migrate.StatusDeferred {
			a.log("aplazado hasta el final del lote: %s (%v)", ev.File.Rel, ev.Err)
		}
		fe := FileEvent{
			Path: ev.File.Rel, Size: ev.File.Size, Bytes: ev.Bytes, Status: ev.Status,
			Millis: ev.Elapsed.Milliseconds(), Attempt: ev.Attempt, Index: i, Total: len(batch),
		}
		if ev.Err != nil {
			fe.Error = ev.Err.Error()
		}
		a.emit("file", fe)
	})

	res := BatchResult{
		Done: sum.Done, Failed: sum.Failed, Bytes: sum.Bytes,
		Seconds:   sum.Elapsed.Seconds(),
		Remaining: len(plan.Pending) - sum.Done,
		Stopped:   ctx.Err() != nil,
	}
	if quarantined {
		res.Remaining = len(plan.Pending) // los aparcados no contaban como pendientes
	}
	if sum.Aborted != nil {
		res.Error = fmt.Sprintf("%v (tras %d ficheros correctos)", sum.Aborted, sum.Done)
		res.AuthLost = dropbox.IsAuthError(sum.Aborted)
		if res.AuthLost {
			a.noteDropboxAuthLost(sum.Aborted)
			res.Error = fmt.Sprintf("%s (tras %d ficheros correctos)", dropboxAuthLostMsg(sum.Aborted), sum.Done)
		}
		a.log("lote abortado: %v", sum.Aborted)
		return res
	}
	a.log("lote terminado: %d correctos (%s), %d fallidos, quedan %d", res.Done, humanBytes(res.Bytes), res.Failed, res.Remaining)
	return res
}

// firstPlaceholder devuelve la ruta relativa del primer marcador "solo
// online" del lote, o "" si no hay ninguno.
func firstPlaceholder(files []migrate.File) string {
	for _, f := range files {
		if f.Placeholder {
			return f.Rel
		}
	}
	return ""
}

// checkDropbox verifica la autorización antes de un lote con marcadores.
// Devuelve (resultado, true) si el lote no debe empezar.
func (a *App) checkDropbox(ctx context.Context, f *dropbox.Fetcher, sample string) (BatchResult, bool) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	account, err := f.Check(cctx, sample)
	if err == nil {
		a.log("Dropbox: autorización válida (%s), %q existe en la nube", account, sample)
		return BatchResult{}, false
	}
	if dropbox.IsAuthError(err) {
		a.noteDropboxAuthLost(err)
		return BatchResult{Error: dropboxAuthLostMsg(err), AuthLost: true}, true
	}
	a.log("Dropbox: %v", err)
	return BatchResult{Error: "Dropbox: " + err.Error() + ". El lote no ha empezado; nada se ha perdido."}, true
}

// dropboxAuthLostMsg redacta el aviso de la interfaz para un fallo de autorización.
func dropboxAuthLostMsg(err error) string {
	var ae *dropbox.AuthError
	if errors.As(err, &ae) && ae.Scope {
		return "La app de Dropbox no tiene permisos suficientes: en dropbox.com/developers/apps, pestaña Permissions, marca files.metadata.read y files.content.read, pulsa Submit y vuelve a pulsar \"Conectar con Dropbox\" (los permisos nuevos solo se aplican al autorizar de nuevo). Los ficheros \"solo online\" siguen pendientes; nada se ha perdido."
	}
	return "Dropbox ha revocado la autorización de esta app: vuelve a pulsar \"Conectar con Dropbox\" en Configuración. Los ficheros \"solo online\" siguen pendientes; nada se ha perdido."
}

// noteDropboxAuthLost registra en la configuración que el token ya no sirve
// para que la interfaz lo muestre también tras reiniciar la app.
func (a *App) noteDropboxAuthLost(err error) {
	a.mu.Lock()
	a.cfg.DropboxAuthError = err.Error()
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persistConfig(cfg)
	a.emit("dropbox:auth-lost", err.Error())
	a.log("Dropbox: %v", err)
}

// ---------- verificación ----------

// StartVerify comprueba en segundo plano lo ya migrado. Emite
// "verify:progress", "verify:fail" y "verify:end".
func (a *App) StartVerify(hash bool) error {
	a.mu.Lock()
	if a.busy != "" {
		a.mu.Unlock()
		return fmt.Errorf("ya hay una operación en curso (%s)", a.busy)
	}
	cfg := a.cfg
	st, err := a.openState(cfg.StatePath)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.busy = "verify"
	a.cancel = cancel
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.busy = ""
			a.cancel = nil
			a.mu.Unlock()
			cancel()
			a.plan = nil
		}()
		client, err := a.client()
		if err != nil {
			a.emit("verify:end", map[string]any{"ok": 0, "bad": 0, "error": err.Error()})
			return
		}
		m := migrate.New(client, st, migrate.Options{RemoteRoot: cfg.RemoteRoot})
		mode := "tamaño"
		if hash {
			mode = "tamaño y hash"
		}
		a.log("verificando %d registros por %s", st.Len(), mode)
		n := 0
		ok, bad := m.VerifyDone(ctx, hash, func(r migrate.VerifyResult) {
			n++
			if !r.OK {
				a.emit("verify:fail", map[string]any{"path": r.Record.Path, "error": r.Err.Error()})
			}
			if n%10 == 0 {
				a.emit("verify:progress", map[string]any{"checked": n})
			}
		})
		a.log("verificación terminada: %d correctos, %d con problemas", ok, bad)
		a.emit("verify:end", map[string]any{"ok": ok, "bad": bad, "stopped": ctx.Err() != nil})
	}()
	return nil
}

// ---------- Dropbox (ficheros "solo online") ----------

// DropboxConnect ejecuta el flujo OAuth con PKCE: abre el navegador, espera
// la autorización en un servidor local y guarda el token de refresco.
func (a *App) DropboxConnect(appKey string) (string, error) {
	appKey = strings.TrimSpace(appKey)
	if appKey == "" {
		return "", errors.New("introduce la App key de tu aplicación de Dropbox")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()
	a.log("abriendo el navegador para autorizar Dropbox…")
	tokens, err := dropbox.LoginViaBrowser(ctx, appKey, func(u string) {
		runtime.BrowserOpenURL(a.ctx, u)
	})
	if err != nil {
		return "", err
	}

	account := ""
	c := dropbox.New(appKey, tokens.RefreshToken)
	if acc, err := c.CurrentAccount(ctx); err == nil {
		account = acc.Email
	}

	a.mu.Lock()
	a.cfg.DropboxAppKey = appKey
	a.cfg.DropboxRefreshToken = tokens.RefreshToken
	a.cfg.DropboxAccount = account
	a.cfg.DropboxAuthError = ""
	a.cfg.FetchOnlineOnly = true
	cfg := a.cfg
	a.plan = nil
	a.mu.Unlock()
	if err := a.persistConfig(cfg); err != nil {
		return "", err
	}
	msg := "Dropbox conectado"
	if account != "" {
		msg += " como " + account
	}
	a.log("%s", msg)
	return msg, nil
}

// DropboxDisconnect olvida el token de refresco.
func (a *App) DropboxDisconnect() error {
	a.mu.Lock()
	a.cfg.DropboxRefreshToken = ""
	a.cfg.DropboxAccount = ""
	a.cfg.DropboxAuthError = ""
	a.cfg.FetchOnlineOnly = false
	cfg := a.cfg
	a.plan = nil
	a.mu.Unlock()
	a.log("Dropbox desconectado")
	return a.persistConfig(cfg)
}

// DropboxStatus comprueba el token contra la API y describe el estado.
func (a *App) DropboxStatus() (string, error) {
	cfg := a.GetConfig()
	if cfg.DropboxRefreshToken == "" {
		return "sin conectar", nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	c := dropbox.New(cfg.DropboxAppKey, cfg.DropboxRefreshToken)
	acc, err := c.CurrentAccount(ctx)
	if err != nil {
		if dropbox.IsAuthError(err) {
			a.noteDropboxAuthLost(err)
			return "", errors.New("autorización revocada: vuelve a conectar")
		}
		if cfg.DropboxAccount != "" {
			return "", fmt.Errorf("token guardado para %s pero la API no responde: %v", cfg.DropboxAccount, err)
		}
		return "", fmt.Errorf("token guardado pero la API no responde: %v", err)
	}
	a.mu.Lock()
	if a.cfg.DropboxAuthError != "" {
		a.cfg.DropboxAuthError = ""
		cfg = a.cfg
		a.mu.Unlock()
		_ = a.persistConfig(cfg)
	} else {
		a.mu.Unlock()
	}
	return "conectado como " + acc.Email, nil
}

// DropboxRedirectURI devuelve la URI que hay que registrar en la App Console.
func (a *App) DropboxRedirectURI() string {
	return dropbox.RedirectURI
}

func (a *App) persistConfig(cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.cfgPath, b, 0o600)
}

// ---------- programación ----------

func (a *App) applySchedule() {
	a.mu.Lock()
	if a.schedStop != nil {
		close(a.schedStop)
		a.schedStop = nil
	}
	minutes := a.cfg.ScheduleMinutes
	if minutes <= 0 {
		a.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	a.schedStop = stop
	a.mu.Unlock()

	a.log("programación activa: un lote cada %d min mientras la app esté abierta", minutes)
	go func() {
		t := time.NewTicker(time.Duration(minutes) * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if a.IsBusy() != "" {
					a.log("programación: operación en curso, se salta este turno")
					continue
				}
				a.emit("schedule:tick", time.Now().Format("15:04:05"))
				if err := a.startBatch("programado", false, false); err != nil {
					a.log("programación: %v", err)
				}
			}
		}
	}()
}

// ---------- utilidades ----------

func (a *App) emit(name string, data any) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, name, data)
	}
}

func (a *App) log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	a.emit("log", map[string]string{"time": time.Now().Format("15:04:05"), "msg": msg})
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// findComposeDir busca docker-compose.yml subiendo desde el directorio actual
// (útil en desarrollo) y, si no, en la carpeta del proyecto en el home.
func findComposeDir() string {
	if wd, err := os.Getwd(); err == nil {
		for d := wd; d != "/" && d != "."; d = filepath.Dir(d) {
			if fileExists(filepath.Join(d, "docker-compose.yml")) {
				return d
			}
		}
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, "PPCL", "INTERNXT"); fileExists(filepath.Join(p, "docker-compose.yml")) {
		return p
	}
	return ""
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
