# stxmix

The Stacks API (the Hiro Stacks Blockchain API under `/extended` and the node RPC under `/v2`, `/v3`) through the
[Nym mixnet](https://nym.com). One Go binary, no dependencies outside the standard library, MIT.

```
stxmix nym setup                         # starts nym-socks5-client on a random active exit gateway
stxmix serve                             # local gateway: http://127.0.0.1:3999 (mainnet), /testnet (testnet)
stxmix info                              # chain tip: Stacks block, Bitcoin block, tenure, block time
stxmix balance SP...                     # STX balance, nonce and fungible tokens
stxmix call-read SM3V...sbtc-token get-total-supply   # read-only contract call, result decoded
stxmix broadcast tx.hex                  # forward an already signed tx; stxmix holds no keys
stxmix probe                             # which public upstreams answer Nym exit traffic, and how fast
stxmix ip                                # the IP that upstreams see (a Nym exit gateway)
```

Built-in networks: `mainnet` (`api.hiro.so`, `api.mainnet.hiro.so`, `stacks-node-api.mainnet.stacks.co`, tried in
order) and `testnet` (`api.testnet.hiro.so`). Add your own node or a devnet with `--add devnet=https://host|https://host2`,
or replace a built-in list with `--add mainnet=https://my-node.example`.

## Why

A Stacks wallet asks its API about your addresses all the time: STX and token balances, nonces before every
signature, contract read-only calls for every DeFi screen, and finally `POST /v2/transactions`. The API operator sees
all of that next to your IP, so it learns which IP holds which addresses, and which IP sent which transaction.
Almost every Stacks wallet and app talks to the same operator, Hiro. Leather, stacks.js, Clarinet and curl all let
you set the API URL, but none of them has a proxy setting. `stxmix serve` gives you a plain `http://127.0.0.1:3999`
to put in that field. The API then sees a Nym exit gateway, not you.

## Use

```
export STXMIX_NYM_SOCKS5=/path/to/nym-socks5-client    # official Nym release binary
go install github.com/apeirs/stxmix/cmd/stxmix@latest
stxmix nym setup            # SOCKS5 on 127.0.0.1:1087 (--port to change; then set STXMIX_PROXY)
stxmix serve &
```

The gateway serves every network on one port, and optionally testnet at the root of a second one:

| What | URL |
|---|---|
| Mainnet (the `--default` network), at the root | `http://127.0.0.1:3999` |
| Any network by name | `http://127.0.0.1:3999/mainnet`, `http://127.0.0.1:3999/testnet`, `/<name>` for `--add` ones |
| Optional testnet listener (`--testnet-listen 127.0.0.1:3998`) | `http://127.0.0.1:3998` |

Below each base URL the paths are the Stacks API's own: `/extended/...`, `/v2/info`, `/v2/accounts/<principal>`,
`/v2/contracts/call-read/...`, `POST /v2/transactions`, `/extended/v1/ws` (websocket). Query strings and POST bodies
go upstream byte for byte. An API's own answer (a 400 for a rejected transaction, a 404) comes back as it is.

Put the base URL into your client:

- **stacks.js** (tested with `@stacks/network` and `@stacks/transactions` 7.7.0):
  `createNetwork({ network: 'mainnet', client: { baseUrl: 'http://127.0.0.1:3999' } })`, and for testnet
  `{ network: 'testnet', client: { baseUrl: 'http://127.0.0.1:3999/testnet' } }`. Read-only calls, `fetchAbi`,
  `fetchNonce` and `broadcastTransaction` all went through the gateway (`examples/stacksjs-client.mjs`).
  **One catch:** `fetchFeeEstimateTransfer({ transaction: <byte count>, network })` ignores `network` and calls
  `https://api.mainnet.hiro.so` directly, over the clearnet. I saw it happen in my live run: that request never
  reached the gateway. Pass `client: { baseUrl }` as well and it stays on the mixnet. A second path is worse: when
  `/v2/fees/transaction` has no estimate, the builders (`makeSTXTokenTransfer`, `makeContractCall`, ...) fall back
  to that same function *without* your `client`, so the request goes to Hiro directly even if you passed one.
  Set `fee` yourself in the builder options to skip estimation. Reported as
  [stx-labs/stacks.js#1891](https://github.com/stx-labs/stacks.js/issues/1891). Watch the gateway log when you
  wire up a new app: any request that doesn't show up there did not go through Nym.
- **curl** (tested): `curl http://127.0.0.1:3999/v2/info`,
  `curl http://127.0.0.1:3999/extended/v1/address/SP.../balances`.
- **Websocket** (tested with Node's built-in WebSocket): `ws://127.0.0.1:3999/extended/v1/ws`, then subscribe as
  usual (`examples/ws-client.mjs`). socket.io's websocket transport (`/socket.io/?EIO=4&transport=websocket`) passes
  through the same way.
- **Leather**: add a custom network with Stacks API URL `http://127.0.0.1:3999` (or `/testnet`). Requests from
  browser extensions are allowed. I have not yet tested this in the Leather UI.
- **Clarinet / devtools** (not yet tested): anywhere they take a Stacks API URL, use the same base URL.

stxmix forwards the request; it never signs anything. `stxmix broadcast` takes an already signed transaction in hex
(a file, an argument or `-` for stdin), checks that its version byte and chain id match the network, and posts the raw
bytes to `/v2/transactions`. `call-read` arguments can be serialized Clarity hex (`0x...`) or short forms: `u42`,
`-7`, `true`, `none`, `'SP...` / `'SP....contract`, `"ascii"`. The result is decoded to Clarity syntax.

**Hiro API key (optional).** Without a key, Hiro limits requests per client IP, and that IP is the Nym exit, which
other Nym users share. If you have a key, set `STXMIX_HIRO_KEY`. stxmix sends it as `X-Api-Key` to `*.hiro.so` only,
also after a redirect, and never logs it. A key the client sends itself is dropped, like every other caller
header. Be aware that a key is tied to your Hiro account, so Hiro can link every request that carries it.

## How it uses the mixnet

- It drives the official `nym-socks5-client` and does no mixing itself. `nym setup` picks a random active exit
  gateway from the Nym API (one that runs an exit-policy network requester), with reply SURBs on, so the exit
  doesn't learn your mixnet address.
- Each upstream connection is a SOCKS5 stream through the entry gateway, three mix layers and the exit. TLS runs end
  to end with the API. Keep-alive reuses a stream, so a wallet polling `/v2/info` doesn't pay a new handshake every
  time.
- API hostnames are resolved by DNS-over-HTTPS *through the mixnet*, then connected by IPv4. The local resolver
  never learns which API you use, and a broken IPv6 route at the exit can't stall a request.
- There is no clearnet fallback. With the mixnet client down, the gateway answers 502
  (`{"error": "...", "reason": "MixnetUnavailable"}`) and the CLI refuses to run.
- Failover: a timeout, a connection error, HTTP 5xx or 429 moves on to the next upstream. A POST is re-sent with
  its body; re-broadcasting the same signed transaction is harmless (same txid). Redirects are followed over the
  mixnet, and only to https (`stacks-node-api.mainnet.stacks.co` answers with a 308 to `api.mainnet.hiro.so`).
- Nothing from the local caller is forwarded. Only a fixed `User-Agent`, `Accept` and the body's `Content-Type`
  (JSON, octet-stream or text) go upstream: no `Origin`, `Cookie`, `Referer`, `Authorization`, `X-Forwarded-For`,
  language or `x-hiro-*` headers. Only `Content-Type` comes back, never `Set-Cookie` or `Location`.
- The gateway listens on loopback and rejects a foreign `Host` header (DNS-rebinding guard). It also rejects any
  web-page `Origin`, so a site you visit can't use your gateway. Browser extensions and non-browser clients are
  allowed; `--allow-origin` adds more.
- Upstreams must be https. Logs name the network, the method, a route template
  (`/extended/v1/address/*/balances`, `/v2/contracts/call-read/*/*/*`), the upstream host and the timing.
  Principals, contract ids, function names, arguments, txids, tx bytes and query strings are never logged, not
  even in error messages.
- stxmix never holds a key. `broadcast` only forwards a transaction that is already signed.

## Limits (read these)

- **It hides your IP, not your addresses.** The API still sees which principals you query, which contracts you
  call and which transactions you send, and everything on chain stays public. stxmix removes the IP link, not the
  on-chain one.
- **One exit IP per client.** All requests leave through the same exit gateway, so the API can link your requests
  to each other ("one user asked about these five addresses"). For unlinkable sessions, run separate client
  identities (`stxmix nym setup --id other --port 1088`).
- **Every built-in upstream is Hiro.** I found no other public, keyless Stacks API: `stacks-node-api.mainnet.stacks.co`
  redirects to Hiro, the old testnet name is gone (404), and the other providers want an account. Failover covers a
  failing hostname or edge, not Hiro itself. If you run a node, add it with `--add mainnet=https://your-node`.
- **Latency is seconds.** In my live run a CLI command on a fresh mixnet stream took 5 to 23 s (median 10 s), and a
  request through the running gateway on a reused stream took about 1.3 s (median of 64, slowest 10.5 s). That is
  fine for balances, contract reads and broadcasts, and slow for an app that fires hundreds of queries.
- **Shared rate limits.** Keyless Hiro traffic is limited per IP, and the exit IP is shared with other Nym users. I
  hit no 429 in my run. If you do, stxmix tries the next upstream, but all of them are Hiro.
- **Clients that ignore your URL bypass it.** See the stacks.js fee-estimate catch above. stxmix only protects
  requests that reach it. The wallet's other traffic (price feeds, token metadata, NFT images) isn't covered either.
- Not tested yet: the Leather UI, Clarinet. Only Hiro's `/extended` and the node's `/v2`/`/v3` paths are covered;
  other indexers aren't built in.

## Tests

`go vet ./... && go test ./...` runs offline against a fake mixnet: a recording SOCKS5 proxy, a DoH server and TLS
APIs. The tests check that:

- every connection goes through the proxy, and the only name lookups are DoH over it;
- GET paths and query strings, and POST bodies (raw tx bytes, call-read JSON), pass through byte for byte;
- `/mainnet`, `/testnet`, custom networks, the root default and the second listener route correctly;
- failover covers 5xx, 429 and timeouts, POSTs included;
- redirects are followed only to https;
- request and response headers are stripped;
- the Hiro key goes to `*.hiro.so` only, across redirects and websocket upgrades, and never into a log;
- the Host and Origin guards hold, preflight included, and encoded dot segments are refused;
- with no proxy there is a 502 and no fallback, in both DNS modes, even when a direct connection would work;
- only https upstreams are accepted;
- no params reach the logs, on success or failure;
- the websocket splice works;
- c32check addresses and Clarity values encode and decode correctly.

I also broke each guard on purpose (22 mutations: Host, Origin, header stripping, key scoping, https-only,
failover, timeouts, log redaction, local DNS, a planted clearnet fallback in the dialer and in the HTTP client, and
more) and checked that a test fails every time.

Live logs from the real Nym mainnet mixnet are in `examples/`.
