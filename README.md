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
machine runs this binary) and create a game. The lobby shows one QR code /
link for the game; everyone opens it on their own phone, taps "Join the
Game" and types their name. The lobby's player list updates live, and the
host starts the game from the same screen. The same link keeps working
after the start: a late joiner takes the last seat in the turn order and
starts with €100.

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
- Only the current player's phone can roll the dice, start/reveal a
  challenge, end the game early, and advance to the next turn — the
  buttons are disabled on everyone else's phone (and the server rejects
  the request even if called directly). Judging success/fail is the one
  action any player can do, so the group can agree on it together.
- Starting the game itself is a separate role: whoever's phone created
  the game is the host, and only that phone (identified by a secret
  token kept in its URL) can start it. Don't share the host's own
  address bar — the lobby's QR code / link doesn't carry that token.
- Mental tasks stay hidden from the player performing them until every
  other player has bet, so they can't get a head start on the puzzle.
- The dice only move players (and pay €50 for passing START). The kind of
  challenge — skill, mental or blind bet — is dealt from a shuffled bag of
  six (two of each), so no kind comes up more than twice in a row and a
  blind bet is never more than eight challenges away.
- Bets are private: each phone only ever sees the bet it placed itself.
  Other players show up as "thinking…" / "bet placed" until the
  challenge is judged, at which point everyone's bet and payout are
  revealed together.
