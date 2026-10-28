package migrate

// PlanOptions ajusta cómo se clasifican los ficheros.
type PlanOptions struct {
	MaxFileSize      int64 // 0 = sin límite
	SkipPlaceholders bool  // true: los ficheros "solo online" no entran en la cola
	// QuarantineAfter aparca un fichero que ha fallado N veces seguidas con el
	// mismo error: sale de la cola (Plan.Quarantined) hasta que se reintente a
	// propósito. 0 = nunca aparcar. Evita que un fichero que el destino
	// rechaza siempre consuma descarga y tiempo en cada lote.
	QuarantineAfter int
}

// Plan clasifica los ficheros del origen según el diario.
type Plan struct {
	Pending []File // sin subir, cambiados desde la última subida o con fallo previo
	Retries int    // cuántos de Pending son reintentos de fallos
	Changed int    // cuántos de Pending ya estaban subidos pero cambiaron en origen

	Placeholders        int // cuántos de Pending son "solo online" (hay que descargarlos de la nube)
	PlaceholdersSkipped int // "solo online" dejados fuera por SkipPlaceholders
	// EmptyUploads son marcadores "solo online" que una versión anterior (sin
	// descarga de la nube) subió como ficheros vacíos: vuelven a la cola para
	// subir el contenido real.
	EmptyUploads int

	// Quarantined son ficheros aparcados: han fallado QuarantineAfter veces
	// seguidas con el mismo error y no entran en la cola.
	Quarantined      []File
	QuarantinedBytes int64

	Done      int
	DoneBytes int64

	Skipped      int
	SkippedBytes int64

	TooLarge      []File // superan MaxFileSize: se registran como skipped al ejecutar
	TooLargeBytes int64

	TotalFiles int
	TotalBytes int64 // solo cuenta bytes conocidos (los marcadores pesan 0 en disco)
}

// PendingBytes devuelve el tamaño acumulado conocido de lo pendiente.
func (p Plan) PendingBytes() int64 {
	var n int64
	for _, f := range p.Pending {
		n += f.Size
	}
	return n
}

// BuildPlan compara el origen con el diario. Un fichero se considera migrado
// si su último registro es "done" con el mismo tamaño y fecha de modificación.
func BuildPlan(files []File, state *State, opts PlanOptions) Plan {
	var p Plan
	p.TotalFiles = len(files)
	for _, f := range files {
		p.TotalBytes += f.Size
		rec, known := state.Get(f.Rel)

		if opts.MaxFileSize > 0 && f.Size > opts.MaxFileSize {
			if known && rec.Status == StatusSkipped && rec.Size == f.Size {
				p.Skipped++
				p.SkippedBytes += f.Size
			} else {
				p.TooLarge = append(p.TooLarge, f)
				p.TooLargeBytes += f.Size
			}
			continue
		}

		pending := false
		switch {
		case !known:
			pending = true
		case rec.Status == StatusDone && f.Placeholder && rec.Size == 0 && rec.Source == "":
			p.EmptyUploads++
			pending = true
		case rec.Status == StatusDone:
			if rec.Size == f.Size && rec.ModTime == f.ModTime {
				p.Done++
				if rec.Bytes > 0 {
					p.DoneBytes += rec.Bytes
					p.TotalBytes += rec.Bytes - f.Size
				} else {
					p.DoneBytes += f.Size
				}
			} else {
				p.Changed++
				pending = true
			}
		case rec.Status == StatusFailed:
			if opts.QuarantineAfter > 0 && state.FailStreak(f.Rel) >= opts.QuarantineAfter {
				p.Quarantined = append(p.Quarantined, f)
				p.QuarantinedBytes += rec.UploadedBytes()
				continue
			}
			p.Retries++
			pending = true
		case rec.Status == StatusSkipped:
			if rec.Size != f.Size {
				pending = true // cambió de tamaño: se reevalúa
			} else {
				p.Skipped++
				p.SkippedBytes += f.Size
			}
		default:
			pending = true
		}
		if !pending {
			continue
		}
		if f.Placeholder {
			if opts.SkipPlaceholders {
				p.PlaceholdersSkipped++
				continue
			}
			p.Placeholders++
		}
		p.Pending = append(p.Pending, f)
	}
	return p
}

// Batch devuelve el siguiente lote: los primeros ficheros pendientes hasta
// agotar maxFiles o maxBytes (0 = sin límite). Siempre incluye al menos un
// fichero si hay pendientes, aunque supere maxBytes por sí solo. Los
// marcadores "solo online" pesan 0 en disco, así que solo los limita maxFiles.
func (p Plan) Batch(maxFiles int, maxBytes int64) []File {
	var (
		batch []File
		bytes int64
	)
	for _, f := range p.Pending {
		if maxFiles > 0 && len(batch) >= maxFiles {
			break
		}
		if maxBytes > 0 && len(batch) > 0 && bytes+f.Size > maxBytes {
			break
		}
		batch = append(batch, f)
		bytes += f.Size
	}
	return batch
}
