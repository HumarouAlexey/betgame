# Bet You Can't! — multiplayer server

A small Go server (standard library only, no external dependencies) that
replaces the Claude-artifact version. It serves the game to every player's
phone over plain HTTP on your local network — no Claude account, no
publishing, no login wall.

## Build

    go build -o betgame .

This produces a single binary (`betgame` / `betgame.exe`) with the web
client baked in via `go:embed` — nothing else to install or copy around.

## Run

    ./betgame

It starts on port 8080 and prints the addresses to use:

    On this machine:      http://localhost:8080
    On your WiFi network: http://192.168.x.x:8080

Open the WiFi address on your own phone/laptop as the host (make sure your
phone and the players' phones are all on the same WiFi network as whichever
machine runs this binary). Set up the game, then share each player's QR
code / link — those are now real, permanent addresses on your network, so
they'll load directly with no sign-in step.

## Playing over the internet instead of local WiFi

If players aren't on the same network, deploy the same binary to any small
VM (a $5 DigitalOcean/Hetzner box, a free-tier Fly.io app, etc.), open port
8080 (or put it behind a reverse proxy on 80/443), and share that public
address instead. The code doesn't change — only where you run the binary.

## Notes

- Game state lives in memory only; restarting the server clears all games.
  Fine for a one-off game night; if you want it to survive restarts later,
  the natural next step is swapping the in-memory map in main.go for a
  small file or SQLite-backed store.
- `skillTaskCount` / `mentalTaskCount` constants in main.go must match the
  length of the `skillTasks` / `mentalTasks` arrays in web/index.html. If
  you add or remove challenges there, update the two constants here too.
- Any player's phone can roll dice, judge success/fail, and advance turns
  — there's no separate "admin" device, matching how you wanted the
  original version to work.
