Output docs/assets/readme/find.gif

Set Shell bash
Set Width 1440
Set Height 780
Set FontSize 18
Set TypingSpeed 1ms
Set Theme "GitHub Dark"
Set Padding 24
Set Margin 16
Set MarginFill "#11111b"
Set WindowBar Colorful
Set BorderRadius 10
Set CursorBlink false
Set Framerate 12

Hide
Type `export SM_DEMO_ROOT="$(mktemp -d /tmp/sm-readme.XXXXXX)"`
Enter
Type `cp -R docs/demo/. "$SM_DEMO_ROOT/"`
Enter
Type `export SM_SUBAGENT_HOME="$SM_DEMO_ROOT/home" SM_DEMO_INTERACTIVE=1`
Enter
Type `export PATH="$SM_DEMO_ROOT/bin:/usr/bin:/bin"`
Enter
Type `alias sm="/tmp/sm-vhs-demo"`
Enter
Type `export PS1="$ "`
Enter
Type "clear"
Enter
Show
Set TypingSpeed 90ms
Type "sm"
Enter
Wait+Screen /Recent sessions/
Sleep 1s
Down
Sleep 1s
Down
Sleep 700ms
Down
Sleep 700ms
Down
Sleep 700ms
Down
Sleep 700ms
Down
Sleep 700ms
Up
Sleep 1s
Hide
Ctrl+C
Sleep 200ms
Type `/bin/rm -rf "$SM_DEMO_ROOT"`
Enter
