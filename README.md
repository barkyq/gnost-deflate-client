# gnost-deflate-client
go nostr client implementing permessage-deflate websocket compression. Downloads the results of a REQ query to a `.jsonl` file, and prints out some stats related to the compression.

# building
In cloned repository run `go build .` to build the executable `gnost-deflate-client`.

# running
accepts an array of filter json objects on stdin, and saves the returned events to a `.jsonl` file:
``` zsh
echo '[{
"authors":["3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"],
"since":1676600000
}]' | ./gnost-deflate-client --port 443 --scheme wss --host nos.lol --output events.jsonl
```
