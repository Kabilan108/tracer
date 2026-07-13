Require tracer

Output tests/output/list-archive.gif
Output tests/output/list-archive.txt

Set Shell "bash"
Set Width 1200
Set Height 650
Set FontSize 22
Set Padding 20
Set Theme "Catppuccin Mocha"
Set Framerate 30
Set TypingSpeed 40ms
Set CursorBlink false

Type "tracer --archive-root /vault/experiments/2026-03-03-tracer-cli/tests/fixtures/archive list --provider codex --json"
Enter
Sleep 3s
