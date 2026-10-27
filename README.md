# gpt-bridge

Claude Code con tu suscripción de Codex.

`claude-codex` abre el Claude Code instalado en tu equipo y cambia su proveedor
por un puente HTTP local hacia `codex app-server`, autenticado con ChatGPT.
Las skills, sesiones, memoria, hooks, permisos, plugins y herramientas MCP siguen
siendo los de Claude Code. El puente no lee, copia ni renueva tokens de OpenAI:
la autenticación queda en el CLI oficial de Codex.

## Dashboard local de procesos

Desde 0.6.0 puedes monitorear todos los puentes del usuario, incluidas sesiones
abiertas con versiones anteriores, sin reiniciarlas:

```bash
claude-codex --dashboard
# Abre http://127.0.0.1:8787
claude-codex --dashboard --port 8790
```

No inicia Claude ni hace llamadas a modelos. Detecta procesos y descendientes en
Linux/WSL, macOS y Windows, y consulta `CODEX_HOME/logs_2.sqlite` en solo lectura.
Actualiza cada tres segundos; permite pausar, buscar por PID/carpeta/thread y
filtrar por modelo. Muestra memoria RSS del árbol, backends de Codex, threads,
nivel de razonamiento, cambios observados y cobertura temporal de los logs. En
Linux descubre también los `CODEX_HOME` de los puentes activos; en macOS y Windows
usa el `CODEX_HOME` del proceso del dashboard.

El modelo procede de spans estructurados nativos de Codex, asociados al PID del
app-server y a sus padres. No se extrae de prompts, respuestas o menciones de
modelos. Distingue una solicitud observada de una respuesta registrada y nunca
presenta el modelo del alias como evidencia de una inferencia. Los procesos sin
registros muestran **Sin evidencia**. Una señal antigua no demuestra que el
modelo siga generando: `Activo` describe la existencia del proceso. Se descartan
asociaciones con PIDs reutilizados.

Los tokens mostrados son el contexto de la última muestra publicada por Codex,
no una suma de consumo. Las muestras/threads y las barras por modelo **no
representan cuota ni porcentajes de uso de la suscripción**. Los registros pueden
rotar; el monitor muestra la evidencia disponible. Carga el historial en lotes y
después lee filas nuevas. Conserva hasta 300 threads visibles y 200 eventos en
memoria, sin crear una segunda base de datos.

Escucha exclusivamente en loopback y no publica prompts, respuestas, líneas de
comandos ni credenciales. No requiere Python, otro servidor web ni una instalación
de SQLite; el lector SQLite está incluido en el ejecutable.

Desde 0.6.1, **Ver evidencia** muestra para cada thread el archivo SQLite, la
fila, el campo `model`, el PID y la muestra que confirma una respuesta. La sección
**Tokens registrados** permite elegir hoy o el historial disponible y desglosa
entrada sin caché, caché, salida y razonamiento por modelo. La atribución de tokens
usa el `turn_context.model` de cada evento, no el modelo actual de la conversación.

Los desgloses proceden de eventos `token_count` en los rollouts JSONL localizados
por el índice `state_5.sqlite`. El lector es incremental, calcula diferencias entre
contadores acumulados y descarta actualizaciones repetidas. Un thread reanudado en
varios procesos se suma una sola vez. El detalle por thread abarca todo su historial,
no solo el proceso actual. Caché y razonamiento son subconjuntos de entrada y salida:
el total es entrada + salida. El día se delimita con la zona horaria del servidor,
indicada junto al resumen. Se muestra cuántos threads tienen desglose recuperable;
los temporales antiguos pueden no tenerlo. Las cifras no equivalen a cuota del plan.

Desde 0.6.2, una compactación con varias ramas del mismo chat se identifica por
mensajes conservados o por el historial de Claude: se cruza el resumen exacto con
su `compact_boundary` y las huellas de las respuestas anteriores. La lectura es
local y no modifica el historial. Esto permite recuperar el thread correcto,
aunque no sea el principal, incluso después de reiniciar el puente. No se elige
una rama solo porque comparta las mismas instrucciones. Si no hay evidencia
suficiente, el puente conserva los threads y devuelve un error de ambigüedad.

## Instalación y uso

El runtime es un ejecutable nativo en **Go**, sin Python ni dependencias de
runtime adicionales. Funciona en Linux, macOS y Windows; requiere Claude Code
y Codex CLI en `PATH`, autenticado mediante `codex login`.
Para compilar requiere Go 1.23 o posterior; para instalar un binario ya compilado
se admite `install.sh --binary RUTA` o `install.ps1 -Binary RUTA`.
Las dependencias de TOML, SQLite y procesos de Windows están fijadas en `go.mod`
y verificadas mediante `go.sum`.
Implementado con Codex 0.163.0 y Claude Code 2.1.304; la ruta WebSearch
se verificó también con Claude Code 2.1.306.

En Linux y macOS (Bash/zsh):

```bash
git clone https://github.com/Lorenzo2345612/gpt-bridge.git
cd gpt-bridge
codex login
bash install.sh
claude-codex
claude-codex --continue
claude-codex --resume
claude-codex --resume ID_DE_SESION
```

En Windows, desde PowerShell 5.1 o posterior:

```powershell
git clone https://github.com/Lorenzo2345612/gpt-bridge.git
cd gpt-bridge
codex login
powershell -NoProfile -ExecutionPolicy Bypass -File .\install.ps1
# Abre otra terminal o ejecuta la línea de activación que imprime el instalador.
claude-codex --resume
```

`ExecutionPolicy Bypass` se aplica únicamente a ese proceso de instalación.
No requiere permisos de administrador. Windows instala en
`%LOCALAPPDATA%\gpt-bridge`, añade su carpeta `bin` al PATH del usuario y carga
los alias desde el perfil de PowerShell. Para automatizar:

```powershell
.\install.ps1 -Alias mi-gpt -YoloAlias mi-gpt-yolo
.\install.ps1 -NoAliases
.\install.ps1 -Binary .\dist\claude-codex.exe -NoAliases -NoPath
```

`-Prefix` cambia el destino, `-ProfilePath` el perfil y `-NoPath` evita modificar
el PATH. Los alias configurados son para PowerShell; desde CMD puede usarse
`claude-codex.exe`. Se admiten ejecutables nativos de Claude/Codex y sus shims
estándar de npm; estos últimos requieren Node en PATH. No se ejecutan shims
`.cmd` personalizados. El cierre de procesos en Windows usa Job Objects para
limpiar los hijos del puente sin terminar la terminal que lo lanzó.

El instalador es interactivo y pregunta qué alias quieres para el modo normal
(por defecto `gpt`) y para YOLO (por defecto `gpt-yolo`). Escribe `-` para omitir
uno. YOLO añade `--dangerously-skip-permissions`, `CLAUDE_YOLO=1` y compactación
a los 600.000 tokens. No sobrescribe alias existentes, como `yolo` o
`claude-2-yolo`. Guarda los nuevos en `~/.config/gpt-bridge/aliases.sh` y añade
una línea para cargarlos desde `.bashrc` o `.zshrc`, según tu shell.
En Windows guarda su equivalente `aliases.ps1` dentro de la instalación; el
modo YOLO restaura las variables del entorno al terminar.

Para automatizar la instalación:

```bash
bash install.sh --alias mi-gpt --yolo-alias mi-gpt-yolo
# Instalar solo el ejecutable, sin cambiar la configuración del shell:
bash install.sh --no-aliases
# Opcional: elegir otro archivo de inicio del shell:
bash install.sh --alias mi-gpt --yolo-alias mi-gpt-yolo --rc-file ~/.bashrc
```

Al terminar indica el comando `source` para activar los alias en la terminal
actual. Las terminales nuevas los cargan automáticamente.

No crea otro perfil de Claude. Si `CLAUDE_CONFIG_DIR` ya está definido, lo
conserva; si no, lo deja sin definir. Usa por tanto el mismo almacén de sesiones
y las mismas personalizaciones que `claude` ejecutado en ese entorno.
Para reutilizar expresamente otro perfil:

```bash
CLAUDE_CONFIG_DIR="$HOME/.claude-glm" claude-codex --resume
```

Las opciones de Claude pasan al ejecutable original. `--settings` del puente
fusiona únicamente los ajustes de proveedor/modelo con tu configuración;
no sustituye tus hooks ni permisos. Si Claude guarda un modelo GPT en el perfil
compartido, al cerrar normalmente el puente restaura el modelo previo, conservando
los demás ajustes y cualquier selección concurrente de un modelo nativo.
Desde 0.6.3, los `--settings` adicionales (archivos o JSON inline, como los hooks
de Claude Workspaces) se fusionan en un único archivo. Los ajustes de conexión y
modelo del puente tienen prioridad; los hooks y permisos se conservan.
Los agentes que ya tenías instalados siguen
cargándose. Los subagentes sin modelo explícito heredan el modelo actual de
la conversación; no se fuerza un modelo global sobre todos ellos.

Desde 0.6.4, si Codex no encuentra el rollout de un hilo que aún no produjo
ninguna respuesta ni llamada a herramientas, el puente puede reconstruirlo
con el mismo historial enviado por Claude. Conserva el checkpoint huérfano con
extensión `.missing-rollout`. No reemplaza hilos con respuestas confirmadas,
herramientas pendientes o un historial diferente.

Desde 0.6.5, la identificación del hilo tras compactar ignora las respuestas
locales que Claude marca como `<synthetic>` (por ejemplo, `No response requested.`).
Estos textos repetidos no identifican una rama; se utiliza la salida real previa
del modelo y se mantiene el rechazo si esa evidencia también es ambigua.

Desde 0.6.6, una nueva compactación de Claude también dispara y espera la
compactación nativa de Codex, conservando el mismo hilo. Así, el historial antiguo
de Codex no sigue creciendo detrás del resumen ni provoca nuevas compactaciones
inmediatas por sus cifras de uso. Una reescritura del historial sin un resumen
nuevo no vuelve a compactar Codex.

Desde 0.6.7, la identificación del hilo conserva las referencias verificadas a
través de compactaciones sucesivas y cambios temporales de proveedor. Los mensajes
que no coinciden con ningún hilo guardado no desplazan esas referencias del búfer.

Desde 0.6.8, los espacios y saltos de línea exteriores de un resumen no alteran
su identidad de compactación; Claude puede añadir un salto de línea al enviarlo.

Desde 0.6.9, el puente desactiva también los MCP nativos definidos en los
`.codex/config.toml` del proyecto y sus directorios superiores. Claude sigue
ejecutando sus propios MCP; cada hilo nativo de Codex ya no arranca copias de
Plane, Playwright o Portal PO. La configuración compartida no se modifica.

```bash
claude-codex --check
claude-codex --models
claude-codex --model NOMBRE_DEL_CATALOGO
claude-codex --codex-model NOMBRE_DEL_CATALOGO --effort high
CLAUDE_CODEX_MODEL=NOMBRE_DEL_CATALOGO claude-codex
```

Si no eliges modelo, usa el predeterminado del catálogo de tu cuenta. Los nombres
del catálogo se consultan al CLI, no se mantienen en una lista fija. El modelo
debe estar disponible para tu cuenta. Los ajustes del puente van antes de `--`;
para resolver un choque de nombres, coloca las opciones de Claude después:

```bash
claude-codex --effort high -- --resume
```

## Modelos, cambio durante la sesión y subagentes

En cada arranque se consultan todas las páginas del catálogo de tu suscripción.
Se publica el catálogo en `/v1/models` y se genera un menú `/model` con todos los
modelos visibles de esa cuenta. No se añaden modelos de API que tu suscripción
no haya publicado. Para cambiar durante una conversación, abre `/model` o escribe
`/model gpt-6-luna`, por ejemplo. En el menú, `s` cambia solo esa sesión; Enter
guarda la preferencia con el comportamiento habitual de Claude.

El selector se construye mediante `modelPicker`, porque el descubrimiento de
Claude filtra nombres que no contienen `claude` o `anthropic` y no mostraría GPT.
El catálogo se vuelve a consultar al abrir el comando. Un modelo desconocido
produce un error explícito, sin cambiar silenciosamente a otro.

Cada modelo tiene también un subagente temporal de sesión, visible en `/agents`.
Su nombre es `codex-` seguido del modelo, sustituyendo puntos por guiones:

```text
Usa codex-gpt-6-astra para revisar el diseño.
Usa codex-gpt-6-luna y codex-gpt-5-6-luna para dos comprobaciones independientes.
```

Estos subagentes fijan su modelo mediante la definición del agente. Al invocarlos
con Agent, omite el parámetro `model` para conservar ese modelo; el parámetro
tiene precedencia sobre la definición. Los alias normales funcionan también:
Fable se asigna al primer Astra del catálogo y Opus al primer Sol
(o al predeterminado si no hay modelos de esa familia).
Haiku al primer Luna, y Sonnet al primer Terra (o Sol si no hay Terra). Puedes cambiar esas
asignaciones con `CLAUDE_CODEX_OPUS_MODEL`, `CLAUDE_CODEX_FABLE_MODEL`,
`CLAUDE_CODEX_HAIKU_MODEL` y `CLAUDE_CODEX_SONNET_MODEL`.

Se admiten varias conversaciones independientes y tandas de llamadas a
herramientas. Claude mantiene la responsabilidad de ejecutar Agent, programar
los subagentes en primer o segundo plano y aplicar sus límites de concurrencia.
Se probaron dos subagentes reales con modelos distintos y cambio de modelo en
una sesión interactiva.

Sin `--effort`, el puente traduce el esfuerzo de `/effort` y de los ajustes de
Claude a cada turno de Codex. `--effort` fija el esfuerzo durante esa ejecución.
Los niveles admitidos por cada modelo aparecen en `claude-codex --models`; se
rechaza un nivel que no está disponible en el modelo seleccionado.

El alias YOLO que elijas admite las mismas opciones (ejemplo con `gpt-yolo`):

```bash
gpt-yolo --resume
gpt-yolo --model gpt-6-astra
gpt-yolo --models
```

## Qué conserva y qué cambia

Claude Code envía sus instrucciones y esquemas de herramientas al puente. Las
instrucciones `system`/`developer` intercaladas de versiones recientes se incorporan
a las instrucciones del modelo. Las llamadas dinámicas se devuelven como bloques
Anthropic `tool_use`: Claude ejecuta Read, Bash, Skill, MCP, etc., aplica sus hooks
y permisos, y envía `tool_result`. El puente lo devuelve al turno suspendido de
Codex. El puente no ejecuta las herramientas de Claude.

Las respuestas de `AskUserQuestion` conservan su resultado original y se
incorporan también al turno activo como decisión explícita del usuario mediante
`turn/steer`, antes de desbloquear la herramienta. Las cancelaciones conservan
su estado de error; no se convierten en aprobaciones.

Los turnos del modelo usan `externalSandbox`: los permisos de ejecución los
controla Claude Code, incluidas sus restricciones y confirmaciones normales o
`--dangerously-skip-permissions` si el usuario lo selecciona. Las herramientas
internas de Codex permanecen deshabilitadas en la conversación principal. El modo `read-only` interno no debe
presentarse al modelo como una prohibición de usar Write/Bash externos.

`WebSearch` de Claude Code hace una segunda petición al proveedor con la
herramienta de servidor `web_search_20250305`. El puente reconoce esa petición
auxiliar y habilita la búsqueda nativa de Codex únicamente para ella, usando la
misma suscripción. Devuelve un resumen con enlaces Markdown que Claude incorpora
al resultado de `WebSearch`; no fabrica bloques cifrados de Anthropic ni ofrece
sus citas o contadores de búsqueda como equivalentes. Exige que Codex emita un
evento real de búsqueda para considerar exitosa la petición.

Se admite `allowed_domains` mediante el filtro nativo. `blocked_domains` y
`user_location` producen un error explícito porque esta ruta no ofrece una
equivalencia verificada. La traducción está limitada a la petición auxiliar de
Claude: una sola herramienta de servidor de búsqueda, sin mezclar herramientas
personalizadas. `max_uses` de Anthropic no es un límite estricto equivalente en
Codex. La conversación principal sigue llamando a `WebSearch` con los permisos
y hooks de Claude.

Los bloques `tool_addition` y `tool_removal` de los mensajes del sistema se
resuelven en orden: se registran solo las herramientas actualmente disponibles.
Se admiten definiciones inline, referencias a herramientas, herramientas
diferidas y referencias a MCP locales. Las bajas de conectores que ya no existen
en el proveedor actual son idempotentes para permitir reanudar sesiones antiguas.
Los recordatorios con `clear_at: next_user_message` dejan de aplicarse después
del siguiente mensaje de usuario, aunque permanezcan en el historial de Claude.

El adaptador utiliza procesos separados para conversaciones distintas, un token
local aleatorio, y escucha solo en `127.0.0.1` y un puerto libre. Cada ejecución
de `claude-codex` inicia y cierra su propio puente. Los errores y respuestas se
traducen a JSON o SSE. Se soportan imágenes base64/HTTPS y resultados de
herramientas con texto/imágenes. No registra prompts ni credenciales.

El runtime Go conserva huellas del historial para reutilizar conversaciones,
en lugar de otra copia completa de cada historial. El caché local de respuestas
está limitado a 128 entradas y 64 MiB. Desde 0.6.10, el recolector descarga los
ejecutores tras 120 segundos inactivos, con una comprobación cada 30 segundos.
Los hilos identificados conservan su checkpoint y se reanudan con el mismo ID;
descargar el proceso no borra la conversación ni reproduce herramientas pasadas.
No se descargan inferencias en curso ni conversaciones identificadas sin
persistencia. Si Claude sigue ejecutando una herramienta, se puede guardar su
espera y descargar solo el ejecutor de Codex: la herramienta de Claude continúa.
Al llegar su resultado se reanuda el mismo hilo; los reintentos conservan los IDs
de herramienta. Las esperas sin checkpoint permanecen residentes. Antes de
descargar se confirma el checkpoint; si no se puede guardar, el proceso permanece
disponible.

Desde 0.6.11, las instrucciones y el catálogo de herramientas de Claude se envían
a Codex con claves estables: cada fragmento del prompt se identifica por su
contenido y cada herramienta por su nombre. Codex solo reenvía las claves cuyo
valor cambió, así que un cambio en `gitStatus` o en una herramienta ya no copia de
nuevo todo el prompt y el catálogo al historial nativo, que antes crecía decenas
de miles de tokens por turno y adelantaba la compactación. Los hilos tampoco
reciben las instrucciones propias de Codex sobre skills, permisos, modo de
colaboración y entorno, que Claude ya aporta.

Desde 0.6.12, el uso de tokens que recibe Claude incluye una estimación de lo
que Codex aún no ha medido. Codex publica el consumo de una muestra solo después
de recibir el resultado de su herramienta, así que cada respuesta `tool_use`
llevaba el consumo de la muestra anterior y la compactación automática de Claude
iba un resultado de herramienta por detrás (y podía pasarse del umbral). La
estimación cuenta 4 caracteres por token, 800 tokens por imagen y respeta el
truncado de Codex (`truncation_policy`, 10k tokens por salida de herramienta en
los modelos actuales); se reemplaza por el valor real en cuanto Codex lo publica.

Desde 0.6.13, los resultados de `ToolSearch` (`tool_reference`) se aceptan y
activan la herramienta diferida que referencian, como hace la API de Anthropic.
Antes el request fallaba, el puente descartaba el ejecutor y reanudaba el hilo en
otro proceso que reenviaba todo el contexto del host. Con `CLAUDE_CODEX_TRACE=1`
también se registran los errores devueltos a Claude.

Desde 0.6.14, el puente decide qué claves de contexto enviar comparando con lo
que el hilo nativo ya contiene, y guarda esos hashes (sin texto del prompt) en el
checkpoint. Codex solo omite claves sin cambios dentro de un mismo proceso
`app-server`, así que reanudar el hilo tras una descarga por inactividad, un
reinicio o un error volvía a añadir todo el prompt y el catálogo (~77k tokens en
una prueba, ~120k en una sesión real). Si Codex compacta el hilo, el contexto se
envía completo una vez.

Desde 0.6.15, cuando Claude compacta su transcript el puente continúa la
conversación en un hilo nativo nuevo (mismo ejecutor, checkpoint y lease) que
recibe solo el resumen de Claude y el contexto del host, igual que Claude, que
tras compactar solo conserva su resumen. La compactación de Codex conservaba
todos los mensajes de usuario anteriores (cada turno de Claude lo es, incluidas
instantáneas completas de compactaciones previas): en una prueba el hilo quedaba
en 133k tokens tras compactar y ahora en 80k. El modelo ya no conserva memoria
nativa de lo que el resumen omite. El hilo anterior permanece en disco.

Desde 0.6.16, el recap que Claude Code pide al volver tras una ausencia ("The user
stepped away and is coming back…") se trata como petición auxiliar, igual que el
resumen de compactación: hilo efímero, sin checkpoint y sin reutilizar. Antes se
guardaba como rama de la conversación y cada recap volvía a añadir el transcript
completo a ese hilo (215k → 379k caracteres en una sesión real).

`--idle-timeout N` configura los segundos de inactividad; `--idle-timeout 0`
desactiva esta limpieza. El número de conversaciones simultáneas sigue sin máximo
por defecto: `--max-sessions N` fija uno y `--max-sessions 0` lo deja ilimitado.
Cada ejecutor residente de Codex conserva su propio consumo de memoria.

La identidad `metadata.user_id` de Claude y la continuidad del historial fijan
la rama. Los turnos normales, cambios de instrucciones, altas/bajas de herramientas
y cambios de modelo entre turnos reutilizan su mismo thread nativo. Claude y
Codex mantienen IDs distintos; el puente conserva la asociación mientras está
abierto y mantiene las ramas de subagentes y búsquedas separadas.

El transporte registra una única herramienta estable, `cc_dispatch`. El catálogo
actual con los nombres y esquemas originales de Claude y sus instrucciones se
envía como contexto de aplicación con rol developer. Cada bloque se limita a
768 bytes UTF-8, por debajo del truncado nativo de 1.000 tokens por valor. El
manifiesto indica cuántos bloques forman la configuración vigente. Los cambios
llegan mediante `turn/start` o `turn/steer` antes de devolver el resultado de una
herramienta pendiente. Claude sigue validando y ejecutando las herramientas.

Las sesiones identificadas de Claude conservan su thread entre ejecuciones.
Desde 0.5.0, el puente crea un thread persistente (`ephemeral: false`) y guarda
su enlace en `~/.claude/gpt-bridge/sessions/`, o dentro de `CLAUDE_CONFIG_DIR`
si usas otro perfil. `CLAUDE_CODEX_STATE_DIR` permite elegir otra carpeta. La ruta
predeterminada separa también los distintos `CODEX_HOME`.

Los JSON del puente contienen IDs, huellas del historial, llamadas pendientes y
estado de recuperación; no contienen prompts, comandos ni resultados de
herramientas. Codex guarda el historial nativo en su propio almacén. Se escriben
checkpoints antes de inferir, antes de entregar una herramienta y al completar
la respuesta, con reemplazo atómico y sincronización. En Unix la carpeta tiene
modo 0700 y los archivos 0600; en Windows se heredan los permisos del perfil.
Los bloqueos del sistema operativo evitan que dos puentes usen simultáneamente
la misma conversación y se liberan también si un proceso muere.

Al abrir de nuevo una sesión, el puente usa `thread/resume` con el ID guardado.
Si Claude compacta, añade su resumen como la vista actual de la misma conversación,
sin sustituir el historial nativo ni crear otro thread principal. Si hay un turno
nativo esperando una herramienta durante la compactación, lo interrumpe antes de
continuar en ese mismo thread. Una llamada pendiente tras reiniciar se recupera
mediante el resultado guardado por Claude, sin reutilizar los IDs RPC de la conexión
anterior. La petición auxiliar que Claude hace para generar el resumen es efímera
y se mantiene separada del thread principal.

Los enlaces no caducan automáticamente: duran hasta que los borres. No son
archivos de `/tmp`, por lo que sobreviven a reinicios del sistema. No borres el
historial nativo de Codex si quieres conservar esos threads. Un checkpoint
corrupto, un thread eliminado o una conversación ya abierta en otro puente
producen un error; el puente no borra el enlace ni crea silenciosamente otro ID.
Si no puede distinguir varias ramas después de una compactación, rechaza la
operación en lugar de mezclar contextos.

`--no-session-persistence` desactiva también la persistencia del puente para esa
ejecución. Las peticiones sin identidad estable de Claude siguen siendo efímeras.
Los threads de versiones anteriores eran efímeros: la primera apertura con 0.5.0
importa el historial de Claude una vez; las posteriores reanudan el thread guardado.
Un máximo de sesiones configurado puede cerrar procesos libres; sus enlaces
persistentes permiten volver al mismo ID cuando se necesiten.

Para comprobar la reutilización en una ejecución concreta:

```bash
CLAUDE_CODEX_TRACE=1 claude-codex --resume
```

La traza imprime `event=new`, `event=reuse`, `event=resume`, `event=compact` o
`event=retire` (y `event=error` con el error devuelto a Claude), el ID nativo, el
tipo de conversación (`role=primary`, `role=side_request` para resúmenes y recaps,
etc.), una
huella de la identidad del host y el número de conversaciones vivas. No imprime
prompts, resultados de herramientas ni credenciales. La caché de tokens de
OpenAI es independiente de la persistencia del thread: conservar su ID e historial
no garantiza que el proveedor mantenga cacheados todos los tokens.


## Verificación de la migración a Go

```bash
cd gpt-bridge
go test -race ./...
go vet ./...
python3 -m unittest -v test_install.py
go test -run '^$' -bench BenchmarkPrepareLargeHistory -benchmem
# Opcional: hace llamadas reales usando la suscripción.
python3 smoke_go.py
```

Pruebas de instalación en Windows, con un binario compilado para ese sistema:

```powershell
go test ./...
go build -o dist/claude-codex.exe .
.\test_install.ps1 -Binary (Resolve-Path .\dist\claude-codex.exe)
```

Las pruebas usan directorios y perfiles temporales; no modifican el PATH del
usuario. Se verificaron en Linux y Windows (PowerShell 5.1), y se compilaron
binarios para Linux, macOS y Windows en amd64/arm64. La ejecución en macOS y
la conexión real a Codex desde Windows todavía requieren validación en esos
entornos; las pruebas de Windows cubren el runtime, protocolo y los instaladores.

`protocol-fixtures.json` contiene 44 casos sintéticos del traductor Python de
referencia. Las pruebas Go verifican paridad, herramientas concurrentes, SSE,
roles/modelos, caché, imágenes, contexto grande, cancelación y limpieza de procesos.
El comando instalado ejecuta directamente el binario Go; Python solo se usa
como cliente opcional de las pruebas reales.

La suscripción mantiene sus límites. Claude puede mostrar una **estimación de
coste API** para un modelo desconocido; ese número no es una factura de OpenAI.
El puente exige autenticación ChatGPT y rechaza cuentas autenticadas con API key.

## Límites de esta versión

- Es una integración propia, no una función oficial de Claude Code. El protocolo
  experimental del app-server puede cambiar con una actualización de Codex.
- Conserva las sesiones identificadas de Claude y reanuda su thread nativo después
  de cerrar o compactar. La primera importación y las peticiones anónimas envían
  el historial como JSON con roles, herramientas y resultados.
  Si el texto supera los 1.048.576 caracteres que admite `turn/start`, lo añade
  mediante `thread/inject_items` y envía una instrucción corta para continuar.
  Conserva el texto íntegro; no recupera razonamiento interno ni convierte las
  llamadas históricas en herramientas ejecutables.
- Las sesiones vivas conservan el turno nativo entre llamada y resultado de una
  herramienta. Las instrucciones y el catálogo se actualizan en el mismo thread;
  cambiar de modelo entre turnos también lo conserva. Cambiar de modelo con una
  herramienta pendiente requiere terminarla primero. Las llamadas concurrentes
  que llegan juntas se devuelven como
  varios bloques `tool_use`, conservando la relación de cada resultado con su llamada.
- El contexto se obtiene del catálogo local de Codex: actualmente máximo nativo
  de 872.000 tokens y ventana efectiva de 828.400 (reserva del 5%). Claude recibe
  esa ventana efectiva; el alias yolo compacta a los 600.000. `--context-window N`
  permite reducir el máximo nativo. Si los modelos tienen ventanas distintas se
  anuncia a Claude la menor, porque su ajuste es global para cambios y subagentes.
  Sin metadatos se conserva el valor prudente de 200.000.
- `count_tokens` es una estimación, no el tokenizer de Anthropic.
- `max_tokens`, `temperature`, `top_p` y el presupuesto de thinking de Anthropic
  no tienen equivalencia exacta en este transporte y no se aplican. El esfuerzo
  se controla con `/effort` o `--effort`; no se expone razonamiento privado como thinking.
- Se rechazan herramientas de servidor Anthropic excepto la petición auxiliar
  de `WebSearch` descrita arriba, además de prefilling,
  `tool_choice` forzado, stop sequences y formatos de salida estructurada.
  No se promete compatibilidad con todas las funciones exclusivas de Claude.
- Los MCP configurados localmente siguen en Claude. Los conectores alojados de
  claude.ai pueden quedar deshabilitados por Claude al cambiar de proveedor.
- Sin límite de conversaciones por defecto en el puente. `--max-sessions N` fija
  un máximo por lanzador; `0` desactiva ese máximo. Los límites de la cuenta
  de Codex y los recursos del equipo siguen aplicándose;
  los ejecutores inactivos con checkpoint recuperable se descargan después
  de 120 segundos (`--idle-timeout`). El hilo persistente conserva su ID al
  reanudar. Reiniciar no borra sesiones de Claude.

## Verificación manual

```bash
claude-codex --check
# Opcional, consume uso de la suscripción:
python3 smoke_go.py
python3 smoke_question.py --delay 65
python3 smoke_search.py
python3 smoke_reuse.py
python3 smoke_stable_context.py
python3 smoke_persistence.py
python3 smoke_resume_cli.py
```

Prueba manual del cliente real:

```bash
claude-codex -- -p 'Lee AGENTS.md con Read y resume sus instrucciones' \
  --tools Read --allowedTools Read --output-format json
```

El uso de la API pública del app-server y herramientas dinámicas está documentado
en https://learn.chatgpt.com/docs/app-server. El servidor sigue siendo experimental.

## Investigación de compatibilidad (2026-10-20)

Se revisó código y pruebas de estos proyectos como referencia; no se instalaron
ni se ejecutaron sus programas. La implementación de este puente sigue siendo
propia y no incorpora su código.

| Proyecto y revisión | Hallazgo y aplicación |
| --- | --- |
| [CLIProxyAPI, `20c9095`](https://github.com/router-for-me/CLIProxyAPI/tree/20c9095cac089c6a8e40554debe4dd8a19d4c14f) | Sus pruebas del ejecutor Claude cubren cambios de herramientas intercalados, referencias y definiciones inline, y coherencia de alias. Se añadieron casos equivalentes de protocolo; esas pruebas son para su ruta Claude y no demuestran que su traductor Codex resuelva todos los mismos casos. |
| [Claudex, `a659986`](https://github.com/caixiaoshun/claudex/tree/a659986619daff11b08c29a5c1a2ac0209dbb4c7) | Tiene matriz de compatibilidad y pruebas para skills, MCP, sesiones y herramientas. Su auditoría citada usa Claude 2.1.76 y sus tipos de conversión no incluyen estos cambios inline: sirve para diseñar la cobertura, no para suponer compatibilidad con 2.1.304. |
| [claude-codex-proxy, `e22600c`](https://github.com/DNDark12/claude-codex-proxy/tree/e22600cd567661fa05e585ef44ba9369faf671c3) | Usa app-server y registro de alias/esquemas de herramientas. Su arquitectura también permite ejecución nativa en Codex; aquí se conserva la ejecución en Claude porque necesitamos sus skills, hooks y permisos. |

La semántica de las altas, bajas, herramientas diferidas, sustitución de
definiciones y caducidad de recordatorios se contrastó con la
[especificación de Anthropic](https://platform.claude.com/docs/en/build-with-claude/mid-conversation-system-messages).
El [issue 57324 de vLLM](https://github.com/vllm-project/vllm/issues/57324)
reporta el mismo tipo de incompatibilidad con `tool_addition` en clientes recientes.

En nuestra sesión fallida había texto junto a tres bloques `tool_removal`,
retirando conectores de claude.ai. Se validaron las cinco variantes de ese contexto
registradas alrededor del fallo. Las pruebas sintéticas no contienen prompts ni
credenciales del usuario.

## Servidor independiente

Para un cliente externo, crea un archivo privado con un secreto local de al menos
24 caracteres y usa `--serve --token-file RUTA --port 8765`. El cliente debe enviar
el secreto en `x-api-key` o `Authorization: Bearer ...` y usar `/v1/messages`.
No es una clave de OpenAI. No expongas ese puerto a otras máquinas.

Para retirar la instalación, elimina el enlace `~/.local/bin/claude-codex` y la
carpeta `~/.local/share/claude-codex-bridge`. Si configuraste alias, retira también
la línea de carga añadida al archivo de inicio y su archivo `aliases.sh`.
En Windows retira la línea de carga del perfil, la entrada del PATH de usuario
y la carpeta de instalación `%LOCALAPPDATA%\gpt-bridge`. Las sesiones de Claude
se conservan.
