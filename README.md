# gnost-deflate-client
go nostr client implementing [permessage-deflate websocket compression](https://www.rfc-editor.org/rfc/rfc7692#section-7). Downloads the results of a REQ query, saving the results in [jsonl format](https://jsonlines.org/), and logs to `stderr` some stats related to the compression.

## Building
In cloned repository run `go build .` to build the executable `gnost-deflate-client`.

## Running
accepts an array of filter json objects on stdin, and saves the returned events to a `.jsonl` file:
``` zsh
echo '[{
"authors":["3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"],
"since":1676600000
}]' | ./gnost-deflate-client --port 443 --scheme wss --host nos.lol --output events.jsonl
```

#### Compatibility with strfry
Set the output to `-` in order to print the returned events to `stdout`. This can be composed with the `import` command for a local instance of [strfry](https://github.com/hoytech/strfry)
```zsh
echo '[{
"authors":["3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"],
"since":1676600000
}]' | ./gnost-deflate-client --port 443 --scheme wss --host nos.lol --output - | strfry import
```
