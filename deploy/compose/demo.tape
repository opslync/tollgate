# Records deploy/compose/demo.gif for the README. Run from a fresh stack:
#   docker compose up -d --force-recreate tollgate   (resets budget state — /data is tmpfs)
#   vhs demo.tape
# Then, if over 3MB: gifsicle -O3 --lossy=80 -o demo.gif demo.gif

Output demo.gif

Set Shell "bash"
Set FontSize 20
Set Width 1200
Set Height 680
Set Padding 20
Set TypingSpeed 45ms
Set Theme "Dracula"

Hide
Type "cd deploy/compose 2>/dev/null; export PS1='$ '; clear"
Enter
Sleep 500ms
Show

Type `curl -s -o /dev/null -w 'checkout-agent -> HTTP %{http_code}\n' -X POST localhost:8080/v1/messages -H "x-api-key: tg_demo_checkout_0123456789ab" -H 'content-type: application/json' -d '{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'`
Enter
Sleep 1200ms

Type "# Tollgate attributed that call to checkout-agent automatically"
Enter
Sleep 800ms
Type "./demo.sh status"
Enter
Sleep 1800ms

Type "# reports-agent has a tiny $0.003 budget — watch it alert, then hard-block"
Enter
Sleep 800ms
Type "./demo.sh trip"
Enter
Sleep 2500ms

Type "./demo.sh status"
Enter
Sleep 2200ms

Type "# a budget stops one agent's spend. the kill switch stops it outright, mid-incident"
Enter
Sleep 800ms
Type "./demo.sh kill checkout-agent"
Enter
Sleep 1500ms

Type "./demo.sh request checkout-agent"
Enter
Sleep 2000ms
