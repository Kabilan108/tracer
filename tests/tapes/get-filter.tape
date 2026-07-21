Require tracer

Output tests/output/get-filter.gif
Output tests/output/get-filter.txt

Set Shell "bash"
Set Width 1200
Set Height 650
Set FontSize 22
Set Padding 20
Set Theme "Catppuccin Mocha"
Set Framerate 30
Set TypingSpeed 40ms
Set CursorBlink false

Type "tracer --archive-root /vault/experiments/2026-03-03-tracer-cli/tests/fixtures/archive get session-one --tool-output=none | tail -12"
Enter
Sleep 2s
Type "tracer --archive-root /vault/experiments/2026-03-03-tracer-cli/tests/fixtures/archive get session-one --turns=user,agent | tail -8"
Enter
Sleep 3s
