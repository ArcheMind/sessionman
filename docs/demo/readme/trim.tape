Output docs/assets/readme/trim.gif

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
Type `python3 -c 'import json,os,pathlib; p=pathlib.Path(os.environ["SM_DEMO_ROOT"])/"home/.claude/projects/agentx/interactive-resume.jsonl"; rows=[json.loads(x) for x in p.read_text().splitlines()]; p.write_text("".join(json.dumps(dict(r,timestamp="2026-11-15T12:%02d:00Z" % n))+chr(10) for n in range(10) for r in rows))'`
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
Enter
Wait+Screen /Trim to 6 turns/
Sleep 2s
Type "y"
Wait+Screen /Resume session as:/
Sleep 1s
Down
Sleep 2s
Hide
Ctrl+C
Sleep 200ms
Type `/bin/rm -rf "$SM_DEMO_ROOT"`
Enter
