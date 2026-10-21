# gpt-bridge

Claude Code con tu suscripción de Codex.

`claude-codex` abre el Claude Code instalado en tu equipo y cambia su proveedor
por un puente HTTP local hacia `codex app-server`, autenticado con ChatGPT.
Las skills, sesiones, memoria, hooks, permisos, plugins y herramientas MCP siguen
siendo los de Claude Code. El puente no lee, copia ni renueva tokens de OpenAI:
la autenticación queda en el CLI oficial de Codex.

## Instalación y uso

El runtime es un ejecutable nativo en **Go**, sin Python ni dependencias de
runtime adicionales. Requiere Linux, Claude Code y Codex CLI en `PATH`.
Para compilar requiere Go 1.23 o posterior; para instalar un binario ya compilado
se admite `install.sh --binary RUTA`. Solo el lector de TOML tiene una dependencia
externa, fijada en `go.mod` y verificada mediante `go.sum`.
Implementado y probado con Codex 0.163.0 y Claude Code 2.1.304.

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

El instalador es interactivo y pregunta qué alias quieres para el modo normal
(por defecto `gpt`) y para YOLO (por defecto `gpt-yolo`). Escribe `-` para omitir
uno. YOLO añade `--dangerously-skip-permissions`, `CLAUDE_YOLO=1` y compactación
a los 600.000 tokens. No sobrescribe alias existentes, como `yolo` o
`claude-2-yolo`. Guarda los nuevos en `~/.config/gpt-bridge/aliases.sh` y añade
una línea para cargarlos desde `.bashrc` o `.zshrc`, según tu shell.

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
No añadas otro `--settings` con ajustes
de proveedor contradictorios. Los agentes que ya tenías instalados siguen
cargándose. Los subagentes sin modelo explícito heredan el modelo actual de
la conversación; no se fuerza un modelo global sobre todos ellos.

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

El alias `claude-codex-yolo` admite las mismas opciones:

```bash
claude-codex-yolo --resume
claude-codex-yolo --model gpt-6-astra
claude-codex-yolo --models
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
internas de Codex permanecen deshabilitadas. El modo `read-only` interno no debe
presentarse al modelo como una prohibición de usar Write/Bash externos.

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
está limitado a 128 entradas y 64 MiB. Un recolector cierra conversaciones sin
peticiones activas tras 30 minutos, aunque no llegue otra petición. El número
máximo de procesos de conversación sigue siendo configurable con `--max-sessions`.
Cada proceso nativo de Codex conserva su propio consumo de memoria.

## Verificación de la migración a Go

```bash
cd gpt-bridge
go test -race ./...
go vet ./...
go test -run '^$' -bench BenchmarkPrepareLargeHistory -benchmem
# Opcional: hace llamadas reales usando la suscripción.
python3 smoke_go.py
```

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
- Conserva las sesiones de Claude. Al reabrirlas o después de compactar, reconstruye
  el contexto de Codex como un historial JSON con roles, herramientas y resultados.
  Si el texto supera los 1.048.576 caracteres que admite `turn/start`, lo añade
  mediante `thread/inject_items` y envía una instrucción corta para continuar.
  Conserva el texto íntegro; no recupera razonamiento interno ni convierte las
  llamadas históricas en herramientas ejecutables.
- Las sesiones vivas conservan el turno nativo entre llamada y resultado de una
  herramienta. Un cambio de instrucciones, modelo o herramientas puede reconstruir
  el contexto. Las llamadas concurrentes que llegan juntas se devuelven como
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
- Se rechazan explícitamente herramientas de servidor Anthropic, prefilling,
  `tool_choice` forzado, stop sequences y formatos de salida estructurada.
  No se promete compatibilidad con todas las funciones exclusivas de Claude.
- Los MCP configurados localmente siguen en Claude. Los conectores alojados de
  claude.ai pueden quedar deshabilitados por Claude al cambiar de proveedor.
- Hasta 32 conversaciones vivas por lanzador, configurable con `--max-sessions`;
  las inactivas caducan a los 30
  minutos mediante el recolector periódico. Reiniciar no borra sesiones de Claude.

## Verificación manual

```bash
claude-codex --check
# Opcional, consume uso de la suscripción:
python3 smoke_go.py
python3 smoke_question.py --delay 65
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
carpeta `~/.local/share/claude-codex-bridge`. Tus configuraciones no se alteran.
