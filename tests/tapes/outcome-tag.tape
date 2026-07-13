Require tracer

Output tests/output/outcome-tag.gif
Output tests/output/outcome-tag.txt

Set Shell "bash"
Set Width 1200
Set Height 650
Set FontSize 22
Set Padding 20
Set Theme "Catppuccin Mocha"
Set Framerate 30
Set TypingSpeed 40ms
Set CursorBlink false

Type "tracer --archive-root /tmp/tracer-vhs-archive outcome session-one done"
Enter
Type "tracer --archive-root /tmp/tracer-vhs-archive tag session-one gold"
Enter
Type "tracer --archive-root /tmp/tracer-vhs-archive list --outcome done --tag gold --json"
Enter
Sleep 3s
