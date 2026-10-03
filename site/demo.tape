# Lararium cell demo — records the README header GIF.
# Requires: vhs 0.6, a baked template under $LARARIUM_CELL_ROOT/templates,
# the /DATA/lararium-demo/c sudo wrapper, and passwordless sudo (dev bench only).
#   ~/.local/bin/vhs site/demo.tape
Set TypingSpeed 30ms
Set FontSize 13
Set Width 1000
Set Height 560
Set Framerate 12
Set FontFamily "DejaVu Sans Mono"
Set Padding 20

Type "cd ~/lararium" Enter
Sleep 0.5
Type "export PATH=/DATA/lararium-demo:$PATH" Enter
Sleep 0.5
Type "clear" Enter
Sleep 0.5

Type "echo '-- is this machine ready to host cells? --'" Enter
Sleep 0.5
Type "c doctor" Enter
Sleep 3
Type "clear" Enter
Sleep 0.5

Type "echo '-- spin up a sandboxed cell --'" Enter
Sleep 0.5
Type "c create demo" Enter
Sleep 3
Type "c start demo" Enter
Sleep 8
Type "c status demo" Enter
Sleep 3
Type "clear" Enter
Sleep 0.5

Type "echo '-- work happens inside: no routes, no leaks --'" Enter
Sleep 0.5
Type "c run demo -- sh -c 'echo egress door: $http_proxy'" Enter
Sleep 3
Type "c run demo -- curl -s -o /dev/null -w 'coop https fetch: %{http_code}' --max-time 20 https://example.com/" Enter
Sleep 6
Type "echo" Enter
Sleep 0.5
Type "c run demo -- curl -s --max-time 8 --noproxy '*' http://example.com/" Enter
Sleep 12
Type "echo bypass attempt failed: rc=$? — every direct route is gone" Enter
Sleep 3
Type "clear" Enter
Sleep 0.5

Type "echo '-- the host itself is behind the floor --'" Enter
Sleep 0.5
Type "c run demo -- curl -s --max-time 8 -x http://10.91.0.1:3128 http://192.168.8.5/" Enter
Sleep 12
Type "echo reaching the host failed: rc=$? — refused at the proxy floor" Enter
Sleep 3
Type "sudo tail -n 2 /DATA/lararium-demo/proxy/access.log" Enter
Sleep 3.5
Type "clear" Enter
Sleep 0.5

Type "echo '-- and it all unwinds in seconds --'" Enter
Sleep 0.5
Type "c stop demo" Enter
Sleep 4
Type "c destroy demo --yes" Enter
Sleep 5
Type "c list" Enter
Sleep 2
