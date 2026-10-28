package dropbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
)

// DefaultMinFree es el espacio libre que se deja siempre en el disco de los
// temporales: macOS se vuelve inestable con el disco lleno.
const DefaultMinFree int64 = 2 << 30

// diskBudget reparte el espacio libre del disco de temporales entre las
// descargas simultáneas. Antes de descargar, cada una reserva su tamaño; si
// no cabe junto a las reservas en curso, espera a que alguna termine. Sin
// esto, seis hilos descargando vídeos de 7 GB llenaban el disco y todos
// fallaban con "no space left on device".
//
// La reserva cubre solo lo que falta por escribir: conforme el temporal
// crece, el espacio libre real baja en la misma medida, así que la reserva
// se reduce con written para no contar dos veces los bytes ya en disco. Sin
// esto, un vídeo de 6 GB bloqueaba 12 GB y el resto de hilos se pasaba horas
// esperando.
type diskBudget struct {
	mu       sync.Mutex
	reserved int64
	changed  chan struct{}                   // se cierra y renueva en cada liberación
	free     func(dir string) (int64, error) // nil = espacio libre real (statfs)
}

// reservation es el espacio apartado para una descarga.
type reservation struct {
	b         *diskBudget
	remaining int64 // bytes reservados que aún no están en disco
	done      int64 // bytes ya escritos, según written
	once      sync.Once
}

// written informa de que ya hay done bytes del fichero en disco: esa parte
// deja de estar reservada porque el espacio libre ya la descuenta.
func (r *reservation) written(done int64) {
	if r == nil {
		return
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	delta := done - r.done
	if delta <= 0 {
		return
	}
	if delta > r.remaining {
		delta = r.remaining
	}
	r.done = done
	r.remaining -= delta
	r.b.reserved -= delta
}

// release devuelve lo que quedara reservado y despierta a las esperas.
func (r *reservation) release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.b.mu.Lock()
		r.b.reserved -= r.remaining
		r.remaining = 0
		close(r.b.changed)
		r.b.changed = make(chan struct{})
		r.b.mu.Unlock()
	})
}

// errDeferred indica que ahora no cabe pero cabrá cuando terminen otras descargas.
var errDeferred = errors.New("sin espacio en disco por ahora: otras descargas lo ocupan")

// acquire reserva size bytes en dir. Si no caben junto a las reservas en
// curso, espera a que alguna termine; con nowait devuelve errDeferred en vez
// de esperar. Si no cabe ni solo, falla enseguida.
func (b *diskBudget) acquire(ctx context.Context, dir string, size, minFree int64, nowait bool) (*reservation, error) {
	freeFn := b.free
	if freeFn == nil {
		freeFn = diskFree
	}
	for {
		b.mu.Lock()
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		free, err := freeFn(dir)
		if err != nil {
			b.mu.Unlock()
			return nil, err
		}
		if free-b.reserved >= size+minFree {
			b.reserved += size
			b.mu.Unlock()
			return &reservation{b: b, remaining: size}, nil
		}
		if b.reserved == 0 {
			// Ni solo cabe: esperar no sirve de nada.
			b.mu.Unlock()
			return nil, fmt.Errorf("no hay espacio en disco para descargarlo: ocupa %s y quedan %s libres en %s (se dejan %s de margen); libera espacio y reinténtalo",
				humanBytes(size), humanBytes(free), dir, humanBytes(minFree))
		}
		ch := b.changed
		b.mu.Unlock()
		if nowait {
			return nil, errDeferred
		}
		select {
		case <-ch:
		case <-time.After(15 * time.Second): // por si otro proceso libera disco
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// diskFree devuelve los bytes disponibles para el usuario en el volumen de dir.
func diskFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("consultando el espacio libre de %s: %w", dir, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
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
