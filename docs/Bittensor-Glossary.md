# Bittensor Glossary (Plain Words)

New to this? Start with the [Quick Start](Bittensor-Quick-Start.md). This page explains the words.

| Term | Plain meaning |
|---|---|
| Bittensor | A blockchain network that pays crypto for useful work. |
| Subnet / SN25 | One market inside Bittensor. URnetwork is number 25. |
| Alpha / $UR | SN25's token. |
| TAO | Bittensor's main coin. It pays for burns and gas. |
| Coldkey | Your main wallet. The money goes here. Keep its secret offline. The address starts with `5`. |
| Hotkey | A working key owned by a coldkey. You only need one for the head tier or hotkey sign-in. |
| Seed / recovery phrase | The secret behind a key. Never put it on a node or in a command line. |
| Network wallet consent | One signature from your coldkey that says "pay all nodes on my URnetwork account to this coldkey". This is the thing that gets you paid. |
| Login-proof wallet | The older website or app wallet link. It shows your address but payouts do not use it. |
| Per-provider consent | A consent for one node only. It wins over the network consent for that node. |
| Hotkey delegation | Pays a hotkey's account to a coldkey across many operators. Rarely needed. |
| Operator | A company running URnetwork servers. Today there is one: URnetwork (`bringyour.com`). |
| Pool tier | The default. Your nodes earn inside the operator's shared pool. |
| Head tier / fleet / Top 200 | Optional. Your own subnet seat, paid directly, ranked by how many different IP ranges you cover. |
| UID | A seat on the subnet. There are 256 in total. |
| Epoch | A 7-day payout period (50,400 blocks). |
| Tempo | A cycle of about 72 minutes in which the network pays seats. |
| Claim | The transaction that moves your credited pool earnings to your coldkey. Anyone can send it, and the money always goes to your coldkey. |
| Claim daemon | A background program that sends claims for you. Optional. |
| Burn | TAO destroyed to register a seat. |
| Gas | A small TAO fee for sending a blockchain transaction. |
| Manifest | The head-tier fleet file: the hotkey plus the member nodes. |
| Commitment oracle | A URnetwork service that copies fleet commitments to where the binding contract can read them. |
| Client ID | One node or proxy slot's identity on URnetwork. |
