# Cómo funciona por dentro

Detalles de diseño de Dropbox a Internxt y el historial de los problemas que
fueron apareciendo durante una migración real de unos 51.000 ficheros. El
[README](../README.md) explica el uso; este documento explica el porqué.

## El diario y el plan

Todo gira alrededor de un diario JSONL de solo añadido (`diario.jsonl`). Cada
línea es un registro con la ruta relativa, el estado (`done`, `failed`,
`skipped`), el tamaño y la fecha de modificación del origen, los bytes
realmente subidos, la ruta remota, el origen del contenido (`local`,
`dropbox-api`, `existente`), el SHA-256 si se calculó y el error si lo hubo.

En cada escaneo se recorre la carpeta de Dropbox y se compara con el último
registro de cada ruta:

- **Migrado**: último registro `done` con el mismo tamaño y la misma fecha de
  modificación. Si el fichero cambia en Dropbox después, vuelve a la cola.
- **Pendiente**: sin registro, modificado, o con un fallo que aún se reintenta.
- **Aparcado**: ha fallado `N` lotes seguidos con el mismo error (ver más abajo).
- **Excluido**: supera el tamaño máximo o casa con una exclusión.

El plan ordena los pendientes y corta el lote por número de ficheros y por
volumen. Los marcadores "solo online" pesan 0 bytes en disco, así que el
límite en MB no los contempla: el límite por número de ficheros es el que
acota esos lotes. Su tamaño real se conoce al descargarlos y queda en `bytes`.

```
Dropbox local ──scan──► plan (pendientes) ──lote──► PUT WebDAV ──► PROPFIND tamaño ──► GET + SHA-256
                              ▲                                                            │
                              └──────────────── diario.jsonl (solo añadido) ◄──────────────┘
```

## Verificación

Tras cada subida se hace un `PROPFIND` y se comprueba el tamaño remoto. Con
verificación por hash, además se descarga el fichero y se compara su SHA-256
con el del origen. El WebDAV de Internxt no expone hashes, así que la
descarga es la única comprobación de contenido real. El subcomando `verify`
(botón "Verificar" en la app) repite esa comprobación sobre todo lo migrado y
devuelve a la cola lo que no cuadre.

## Ficheros que ya están en Internxt

Antes de subir, se consulta el destino. Si el fichero existe con el mismo
tamaño (y el mismo hash, con verificación por hash), se adopta sin subirlo y se
anota con `source: existente`. Esto permite retomar una migración empezada con
la app de escritorio de Internxt o con otra herramienta.

Limitación del WebDAV de Internxt 1.6.9: no localiza por ruta los nombres con
`+ & # ? %` (los pasa sin codificar a su API), así que no puede reemplazarlos
y responde `409 File already exists`. En ese caso se busca el fichero en el
listado de su carpeta y se adopta si el tamaño coincide; si no coincide, falla
con explicación y hay que borrarlo en la web de Internxt o renombrarlo en
Dropbox.

## Nombres con `#` o `?`

El WebDAV de Internxt corta la ruta en esos caracteres (fragmento y query de
URL) y responde "A folder exists on the cloud with the same name". Se
sustituyen en el destino por sus equivalentes de ancho completo `＃` y `？`
(visualmente iguales) en cada segmento de la ruta; el diario guarda la ruta
remota real en `remote`.

## Ficheros "solo online" de Dropbox

El cliente clásico de Dropbox para macOS deja los ficheros no descargados como
marcadores de 0 bytes con el atributo extendido `com.dropbox.placeholder`.
Leerlos no dispara la descarga y no hay API pública para pedírsela al cliente.
La herramienta los detecta y, si está activado, los descarga directamente de la
nube de Dropbox a un temporal, verifica el `content_hash` que anuncia Dropbox,
los sube a Internxt, los verifica y borra el temporal. En disco nunca queda
copia local.

Antes de cada lote con marcadores se comprueba la autorización de Dropbox con
una llamada barata. Si falla, el lote se aborta con un único aviso: ningún
fichero se anota como fallido y todos siguen pendientes. La comprobación
también detecta que la app de Dropbox no tenga los permisos
`files.metadata.read` y `files.content.read` (los permisos nuevos no se
aplican a tokens ya emitidos: hay que volver a autorizar) y que la cuenta
conectada contenga realmente el primer fichero "solo online" del lote (evita
migrar con otra cuenta de Dropbox por error).

**Marcadores subidos vacíos.** Las versiones anteriores a la descarga por API
subían los marcadores como ficheros de 0 bytes. El plan los detecta (registro
`done` con tamaño 0 y sin `source`) y los devuelve a la cola para subir el
contenido real, que reemplaza al vacío.

## Reintentos sin volver a descargar

El origen de un fichero (el local o la descarga temporal de Dropbox) se abre
una vez por fichero y se reutiliza en los reintentos de subida. Antes, cada
reintento de un vídeo de 7 GB volvía a descargarlo entero, con lo que un
fichero que fallaba por `Headers Timeout Error` costaba horas por lote.

## Ficheros aparcados (cuarentena)

Algunos vídeos de cientos de MB se quedaban clavados a mitad de subida
(siempre en el mismo porcentaje) y a los 5 minutos el WebDAV respondía
`HTTP 500 Headers Timeout Error`. Sin cuarentena, cada lote volvía a
descargarlos de Dropbox y a esperar el corte, una vez por reintento, y los
hilos acababan todos atascados en ellos al final de cada lote (huecos de 30-50
minutos sin actividad).

El diario cuenta los fallos seguidos de cada ruta con el mismo error
(`FailStreak`). Cuando llegan a `N` (por defecto 3; `-quarantine N` en la CLI,
"Aparcar tras N lotes fallidos seguidos" en la app), el fichero sale de la cola
y deja de descargarse. Se reintenta solo a propósito, con `run
-retry-quarantined` o el botón "Reintentar aparcados"; si vuelve a fallar,
sigue aparcado. Un éxito, o un error distinto, reinicia la cuenta: por eso un
fichero aparcado que falla con otro error (por ejemplo `broken pipe` al parar
la app a media subida) vuelve a la cola normal.

## Ficheros grandes de uno en uno

La causa real de los `Headers Timeout Error` está en la CLI de Internxt: sube
los ficheros de más de 100 MB en partes de 15 MB, diez a la vez, con el
cliente HTTP `undici`, cuyo `headersTimeout` por defecto es de 300 s. Cada
petición tiene que completarse en ese tiempo. Con varios ficheros grandes
subiendo a la vez por una conexión doméstica (en torno a 1 MB/s de subida) el
ancho de banda se reparte y ninguna parte llega a tiempo. En los registros del
WebDAV, 293 de 311 subidas fallidas habían durado entre 290 y 320 s.

Por eso un fichero de 100 MB o más ocupa todos los turnos de subida: mientras
sube, los demás esperan ("Esperando turno de subida" en la tabla) y las
descargas de Dropbox continúan en paralelo. Los pequeños siguen subiendo en
paralelo entre sí. El turno se libera al acabar la subida, antes de la
verificación. Con este cambio, los 40 ficheros de 137 a 867 MB que llevaban
días aparcados subieron del tirón, sin un solo fallo.

## Espacio en disco para los temporales

Cada fichero "solo online" se descarga entero a un temporal antes de subirlo.
Con 6 hilos y vídeos de 7 GB el disco de arranque se llenaba (`no space left
on device`) y todas las descargas fallaban a la vez. El descargador consulta
ahora el tamaño antes de empezar y reserva ese espacio: si no cabe junto a las
descargas en curso, espera a que alguna termine, y si no cabe ni solo, falla
enseguida indicando el tamaño y el espacio libre. Siempre deja 2 GB libres.

La reserva cubre solo lo que falta por escribir: conforme el temporal crece se
va liberando, porque el espacio libre real ya descuenta esos bytes (sin esto
un vídeo de 6 GB bloqueaba 12 GB). Mientras queden otros ficheros en el lote,
un hilo que no tiene sitio para el suyo no se queda esperando: el fichero se
aplaza al final del lote (evento `deferred`, sin anotar nada en el diario) y el
hilo sigue con el siguiente; los aplazados se retoman cuando la cola se agota.

Ojo con las instantáneas locales de Time Machine: retienen los temporales
borrados hasta una hora o más, y el espacio no vuelve hasta que macOS las purga
(`tmutil listlocalsnapshots /` y `tmutil deletelocalsnapshots <fecha>`).

## Exclusiones

Las exclusiones son patrones glob. Sin `/` inicial, el patrón se aplica al
nombre en cualquier carpeta y a la ruta relativa; un patrón de carpeta
(`Fotos/RAW`) excluye todo su contenido. Con `/` inicial, se aplica solo a la
ruta exacta. El botón "saltar" escribe la ruta exacta anclada y con los
comodines escapados, de modo que no arrastra a otros ficheros con el mismo
nombre. Siempre se excluyen además los metadatos de Dropbox y del Finder
(`.dropbox`, `.dropbox.cache`, `.DS_Store`, `._*`, `Icon\r`, `*.tmp`, `~$*`).

La ventana "Gestionar exclusiones" recorre la carpeta de Dropbox aplicando
solo las exclusiones integradas y mide, para cada patrón del usuario, cuántos
ficheros deja fuera con la misma regla que el escaneo (fichero y carpetas
ancestras). Para las rutas exactas consulta además el diario, para enseñar por
qué se saltó cada fichero.

## Velocidades medidas

Octubre de 2026, conexión doméstica, un hilo: descarga de Dropbox ~2,4 MB/s,
subida a Internxt ~1,2-1,4 MB/s, descarga de Internxt ~6,5 MB/s, más 2-3 s de
latencia por fichero. Con 2 hilos y verificación por hash salen ~400 ficheros
pequeños por hora. La subida es el cuello de botella: 17 GB de vídeos grandes
tardaron unas 5 horas con un solo hilo.

## El gráfico de tarta

El anillo del panel de Estado tiene dos vistas. "Todo" reparte los ficheros en
Migrados, Pendientes, Aparcados y Excluidos (se cuenta por ficheros porque los
"solo online" pesan 0 hasta descargarse). "Lote" muestra la evolución del lote
en curso (correctos, fallidos, omitidos, en curso, aplazados y en espera) con
el porcentaje terminado y cuántos quedan, para saber cuándo es buen momento de
cerrar la app o reiniciar el Mac sin cortar un lote a medias.
