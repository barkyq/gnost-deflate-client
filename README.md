# gnost-deflate-client
go nostr client implementing [permessage-deflate websocket compression](https://www.rfc-editor.org/rfc/rfc7692#section-7). Downloads the results of a REQ query, saving the results in [jsonl format](https://jsonlines.org/), and logs to `stderr` some stats related to the compression.

## Building
In cloned repository run `go build .` to build the executable `gnost-deflate-client`.

## Running
Accepts an array of filter json objects on stdin, and saves the returned events to a `.jsonl` file:
``` zsh
echo '[{
"authors":["3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"],
"since":1676600000
}]' | ./gnost-deflate-client --port 443 --scheme wss --host nos.lol --output events.jsonl
```

#### Keep connection alive
Use the `--keepalive N` option to keep the connection alive, sending a PING frame to the relay every `N` seconds. New returned events will be appended to the `--output` file. Enter `C-c` to cleanly shutdown the listener.

#### Compatibility with relays
Set the output to `-` in order to print the returned events to `stdout`. This can be composed with the `import` command for a local instance of [gnost-relay](https://github.com/barkyq/gnost-relay) or [strfry](https://github.com/hoytech/strfry)
```zsh
echo '[{"since":1676863922,"kinds":[1]}]' |\
gnost-deflate-client --port 443 --scheme wss --host knostr.neutrine.com --keepalive 30 --output - |\
DATABASE_URL=postgres://x gnost-relay --import
```
```zsh
echo '[{
"authors":["3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"],
"since":1676600000
}]' | ./gnost-deflate-client --port 443 --scheme wss --host nos.lol --output - | strfry import
```
Note that `strfry import` is not compatible with `--keepalive` since `strfry` expects an `EOF` from `stdin`.
