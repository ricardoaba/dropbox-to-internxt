package migrate

import "golang.org/x/sys/unix"

// hasXattr indica si el fichero tiene el atributo extendido dado. Con dest nil
// Getxattr solo consulta el tamaño: devuelve error (ENOATTR en macOS, ENODATA
// en Linux) si el atributo no existe.
func hasXattr(path, name string) bool {
	_, err := unix.Getxattr(path, name, nil)
	return err == nil
}
