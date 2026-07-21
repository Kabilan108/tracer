Require tracer

Output tests/output/push-pipeline.gif
Output tests/output/push-pipeline.txt

Set Shell "bash"
Set Width 1200
Set Height 650
Set FontSize 22
Set Padding 20
Set Theme "Catppuccin Mocha"
Set Framerate 30
Set TypingSpeed 40ms
Set CursorBlink false

Env XDG_CONFIG_HOME "/tmp/tracer-vhs-push/config"
Env XDG_STATE_HOME "/tmp/tracer-vhs-push/state"

Type "tracer --archive-root /tmp/tracer-vhs-push/archive push demo --dry-run"
Enter
Sleep 2s
Type "tracer --archive-root /tmp/tracer-vhs-push/archive tag session-one wiki:compiled"
Enter
Type "tracer --archive-root /tmp/tracer-vhs-push/archive list --json --tag '!wiki:compiled'"
Enter
Sleep 3s
