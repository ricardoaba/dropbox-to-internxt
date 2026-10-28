package dropbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/ricardoaba/dropbox-to-internxt/migrate"
)

var _ migrate.Fetcher = (*Fetcher)(nil)

// Fetcher descarga ficheros de la nube de Dropbox a temporales. Implementa
// la interfaz migrate.Fetcher: el migrador lo usa para los marcadores
// "solo online", sube el temporal a Internxt y después lo borra.
type Fetcher struct {
	Client  *Client
	TempDir string // "" = os.TempDir()
	// Root es la carpeta de Dropbox a la que corresponde el origen local,
	// normalmente "" (raíz de la cuenta). Para una subcarpeta: "/Trabajo".
	Root string
	// MinFree es el espacio que siempre se deja libre en el disco de los
	// temporales (0 = DefaultMinFree). Las descargas simultáneas reservan su
	// tamaño y, si no caben, esperan a que otra termine.
	MinFree int64

	budget diskBudget
}

// Check comprueba, con llamadas baratas, que la autorización sigue siendo
// válida y que la cuenta conectada contiene sample (ruta relativa de un
// marcador del lote; "" para omitirlo). Devuelve el email de la cuenta.
func (f *Fetcher) Check(ctx context.Context, sample string) (string, error) {
	acc, err := f.Client.CurrentAccount(ctx)
	if err != nil {
		return "", err
	}
	if sample == "" {
		return acc.Email, nil
	}
	remote := path.Join("/", f.Root, sample)
	if _, err := f.Client.GetMetadata(ctx, remote); err != nil {
		if IsNotFound(err) {
			return acc.Email, fmt.Errorf("la cuenta de Dropbox conectada (%s) no contiene %q: conecta la cuenta a la que pertenece la carpeta local", acc.Email, remote)
		}
		return acc.Email, err
	}
	return acc.Email, nil
}

// Fetch descarga rel (ruta relativa al origen) a un fichero temporal.
// Primero consulta el tamaño y reserva ese espacio en el disco de temporales;
// si no cabe junto a las otras descargas en curso, espera a que terminen.
func (f *Fetcher) Fetch(ctx context.Context, rel string, progress func(done, total int64)) (string, int64, func(), error) {
	dir := f.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	remote := path.Join("/", f.Root, rel)
	meta, err := f.Client.GetMetadata(ctx, remote)
	if err != nil {
		return "", 0, func() {}, f.wrapErr(remote, err)
	}
	minFree := f.MinFree
	if minFree <= 0 {
		minFree = DefaultMinFree
	}
	res, err := f.budget.acquire(ctx, dir, meta.Size, minFree, migrate.CanDefer(ctx))
	if err != nil {
		if errors.Is(err, errDeferred) {
			return "", 0, func() {}, fmt.Errorf("%w: %s ocupa %s y %w", migrate.ErrDefer, rel, humanBytes(meta.Size), err)
		}
		return "", 0, func() {}, err
	}
	if progress != nil {
		progress(0, meta.Size)
	}
	tmp, err := os.CreateTemp(dir, "dropbox2internxt-*"+safeExt(rel))
	if err != nil {
		res.release()
		return "", 0, func() {}, err
	}
	cleanup := func() { os.Remove(tmp.Name()); res.release() }

	// Conforme el temporal crece, la reserva se reduce: el espacio libre ya
	// descuenta esos bytes y así otros hilos no esperan de más.
	track := func(done, total int64) {
		res.written(done)
		if progress != nil {
			progress(done, total)
		}
	}
	meta, err = f.Client.Download(ctx, remote, tmp, track)
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return "", 0, func() {}, f.wrapErr(remote, err)
	}
	return tmp.Name(), meta.Size, cleanup, nil
}

func (f *Fetcher) wrapErr(remote string, err error) error {
	if IsNotFound(err) {
		return fmt.Errorf("no existe en la nube de Dropbox: %s", remote)
	}
	if IsAuthError(err) {
		// Afecta a todos los ficheros del lote: el migrador lo aborta en vez de
		// marcar uno a uno como fallidos.
		return fmt.Errorf("%w: %w", migrate.ErrAbort, err)
	}
	return err
}

func safeExt(rel string) string {
	ext := path.Ext(rel)
	if len(ext) > 16 || strings.ContainsAny(ext, "*?/\\") {
		return ""
	}
	return ext
}
