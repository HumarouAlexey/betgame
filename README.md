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

Open the WiFi address on your own phone/laptop (make sure your phone and
the players' phones are all on the same WiFi network as whichever machine
runs this binary); a new game is created as the page opens. The lobby shows one QR code /
link for the game; everyone opens it on their own phone, taps "Join the
Game" and types their name. The lobby's player list updates live, and any
player who has joined can start the game from the same screen. The same link keeps working
after the start: a late joiner takes the last seat in the turn order and
starts with €100.

## Removing players

The organizer password is `BgaGoSeeWin` (`defaultAdminPassword` in
main.go). To use a different one without editing the code, start the
server with:

    BETGAME_ADMIN_PASSWORD='another password' ./betgame

In a game, tap the logo to open the standings, then "Manage players" and
enter that password: each player gets a Remove button. A removed player
loses their seat and money and can rejoin with the link as a new player;
if it was their turn, the turn passes on. The password is kept only in
that browser tab's memory, so a reload asks for it again.

## Song challenges

In a song challenge a 30-second clip plays on the performer's phone, and
they have to name the artist or band before it ends (the song title isn't
needed). Everyone else sees the answer while betting; the performer sees
it once they tap "I've answered", the clip runs out, or the results are in.

Clips are Deezer's free 30-second previews (no account or API key). Songs
come from two pools: international hits (Deezer's chart, mostly in
English) and Russian hits (three public Deezer playlists: hits of the
2000s, hits of 2025 and Russian rock). If more than half the players have
the page in Russian, each song comes from either pool, half and half;
otherwise every song is from the international pool. The language is
whatever each player has picked with the EN / RU switch.

To use other playlists, start the server with their ids (the number in a
playlist's link). `BETGAME_DEEZER_PLAYLIST` replaces the chart, and
`BETGAME_DEEZER_RU_PLAYLISTS` (comma-separated) replaces the Russian
playlists:

    BETGAME_DEEZER_PLAYLIST=1234567890 BETGAME_DEEZER_RU_PLAYLISTS=111,222 ./betgame

A song doesn't come back, in any game on this server, until every song in
the list has been played. The played songs are remembered in
`played_tracks.json` next to the binary, so a restart doesn't forget them;
delete that file to start over.

The server needs internet access for this. Without it the game works as
before and simply deals no song challenges.

## TV mode

To show the game on a big screen, open the game link in the TV's (or a
laptop's) browser and tap **TV** in the header. That screen then shows
what the current player sees, with the standings and the join QR code
beside it. It can't do anything in the game: players still play on their
phones. Anything hidden from the current player (the song, a mental task
before the bets are in) stays hidden on the TV too, since they're looking
at it.

While a TV is showing the game, song clips play on the TV, once, instead
of on the performer's phone. If the TV is switched off or closed, clips
go back to the phone within a few seconds. Browsers only play sound after
a tap on the page, which the TV switch counts as; if the TV was reloaded
in TV mode, it shows a "Turn on the sound" button instead. Tap **TV**
again to turn the mode off. The switch is only offered on devices that
haven't joined as a player.

## Duels

In a duel the performer sees the task, then picks another player as their
opponent. Everyone else bets on who will win (the opponent doesn't bet).
The performer records the winner, and the winner takes the pot that
would otherwise go to a successful performer. Duels need at least two
players.

## Phone mini-games

Two challenges are played on the performer's phone, which also judges
the result, so nobody has to record it: "exactly 10 seconds" (a skill
task: stop a hidden stopwatch within 0.3 s of ten seconds) and "remember
the colors" (a mental task: repeat a sequence of 7 colors). What the phone
measured is shown with the results.

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
- `skillTaskCount` / `mentalTaskCount` / `duelTaskCount` constants in
  main.go must match the length of the `skillTasks` / `mentalTasks` /
  `duelTasks` arrays in web/index.html. If you add or remove challenges
  there, update the constants here too.
- Only the current player's phone can roll the dice, start/reveal a
  challenge, record whether it succeeded, end the game early, and advance
  to the next turn — the buttons are disabled or hidden on everyone
  else's phone (and the server rejects the request even if called
  directly).
- Any player who has joined can start the game from the lobby; someone
  who has only opened the link can't until they join.
- Mental tasks stay hidden from the player performing them until every
  other player has bet, so they can't get a head start on the puzzle.
- The dice only move players (and pay €50 for passing START). The kind of
  challenge — skill, mental, song, duel or blind bet — is dealt from a
  shuffled bag of ten (two of each), so no kind comes up more than twice in
  a row and a blind bet is never more than sixteen challenges away.
- A player who ends a challenge with €10 or less (in practice €5–10, since
  a lost bet can't take anyone lower) is topped up with €100 by the bank,
  with a message on the results screen.
- Bets are private: each phone only ever sees the bet it placed itself.
  Other players show up as "thinking…" / "bet placed" until the
  challenge is judged, at which point everyone's bet and payout are
  revealed together.
