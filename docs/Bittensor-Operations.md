# Bittensor Subnet 25 Operations & Commands Guide

This guide details operating procedures, command references, and telemetry monitoring for URNetwork Connect nodes participating in Bittensor Subnet 25.

---

## 1. Subnet 25 Architecture & Earning Mechanics

URNetwork Connect providers operate on a dual-incentive model:
1. **Network Provider Traffic**: Direct bandwidth earnings based on billable byte delivery and client session routing.
2. **Bittensor Subnet 25 Incentive Pool**: On-chain emissions distributed proportionally to verified miners based on Merkle-proven bandwidth scores across finalized payout epochs.

### Identity & Wallet Model
- **Network Account Token (`jwt`)**: The provider authenticates to the URNetwork control plane using a signed JWT stored at `~/.urnetwork/jwt` (or container volume `/home/urnet/.urnetwork/jwt`).
- **Claim Coldkey (`ss58`)**: Bittensor coldkey address (prefix `42`, e.g. `5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5`) registered with the platform to authorize and receive pool emissions.
- **Mirror Account**: EVM smart contract accounts mirrored on Subtensor via `evm:<H160_address>` hash derivation for non-custodial claims.

---

## 2. Coldkey Registration & Setup

Before claiming Subnet 25 emissions, link your Bittensor coldkey to your network account using any of the following methods.

### Method 1: Single-Line curl (Direct API)
Run this single command on the machine running your provider:
```bash
curl -s -X POST "https://api.bringyour.com/sn/wallet" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $(cat ~/.urnetwork/jwt)" \
  -d '{"coldkey_ss58": "5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5"}'
```
Returns `{}` on success.

### Method 2: Host / Bare-Metal CLI
```bash
# Register coldkey for the local provider
provider wallet set 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5
```

### Method 3: Docker Deployments
Pass your coldkey directly via `docker-compose.yml` or container flags:
```yaml
services:
  provider:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.30.9
    command: ["provide", "--wallet=5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5"]
    volumes:
      - ur_config_1:/home/urnet/.urnetwork
```

---

## 3. Real-Time Status & Telemetry (`sn-status`)

Use `sn-status` to inspect your global ranking, bandwidth delivery, coldkey linkage, and Top 200 cutoff eligibility in real time without exposing credentials in the process table.

### Commands
```bash
# Host CLI (systemd / bare-metal)
urnet-tools sn-status

# Docker CLI (delegates directly into running container)
urnet-docker sn-status

# Provider binary directly
provider sn-status

# JSON format for monitoring agents and Prometheus scrapers
urnet-tools sn-status --json
```

### Dashboard Output
```text
════════════════════════════════════════════════════════════════════════════════
  URNetwork Subnet 25 — Node & Miner Status
════════════════════════════════════════════════════════════════════════════════
  Provider:         urnetwork.service (State: /var/lib/urnetwork)
  Network:          miner-node-01 (f078e470-36d0-48e0-bb15-b5d1e2e9c1aa)
  Global Rank:      #7 [Public] — Tier 1 Elite (Rank #7 Globally)
  Net Bandwidth:    1,234,567.89 MiB (1205.63 GiB) provided
  Coldkey (SS58):   5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5
  Coldkey (Hex):    0xa21b...
  Subnet Epoch:     #1054 (Start: #1054000, Finalize: #1054720)
  Contract:         0x5FbDB2315678afecb367f032d93F642f64180aa3 (Chain ID: 964)
  Payout Share:     4.85% (485 bps in Epoch #1053)
════════════════════════════════════════════════════════════════════════════════
```

### Ranking Tiers & Eligibility
- **Tier 1 Elite (Rank 1–10)**: Top global emission allocation; highest validator query preference.
- **Tier 2 High-Volume (Rank 11–50)**: High yield; fast validator probe acceptance.
- **Tier 3 Active (Rank 51–200)**: Subnet 25 emission cutoff eligibility boundary.
- **Unranked / Sub-200**: Displays exact distance to Top 200 emission threshold.

---

## 4. Epoch Payout Claims

Subnet 25 uses cryptographic Merkle tree payout roots committed on-chain at the conclusion of each epoch.

### Epoch Stages
1. **Active Epoch ($e$)**: Nodes stream billable bandwidth; validators collect signed trails.
2. **Commit Window**: Validator consensus commits the payout root hash.
3. **Dispute / Finalization**: Payout root is locked on-chain (`finalize_block`).
4. **Claim Window**: Verified claims become redeemable.

### Workflow 1: Air-Gapped / Offline Calldata (Recommended)
Generates ABI-encoded calldata and cryptographic inclusion proofs without exposing private keys on the provider host:
```bash
provider claim --epoch=1053
```
Output yields ready-to-submit calldata compatible with `snclaim` or web3 wallets.

### Workflow 2: Direct On-Chain Submission
Submit the claim transaction directly with a local private key file and RPC endpoint:
```bash
provider claim \
  --epoch=1053 \
  --rpc=https://rpc.subtensor.network \
  --key_file=/path/to/coldkey_evm.key
```

### Dry Run Verification
Simulate verification and root matching without broadcasting transactions:
```bash
provider claim --epoch=1053 --rpc=https://rpc.subtensor.network --dry-run
```

---

## 5. Head Node Delegation (`bind-head`)

Head nodes aggregate proofs from client nodes for on-chain batch verification.

### Binding a Hotkey
```bash
provider bind-head \
  --hotkey=0x1234567890abcdef... \
  --registrant=0xYourEvmAddress... \
  --contract=0xContractAddress...
```

> [!IMPORTANT]
> The `--registrant` address MUST equal the EVM transaction sender. The head-bind digest binds cryptographically to this sender, and the transaction reverts if the sender's Subtensor mirror does not match the hotkey's on-chain coldkey.

### Unbinding a Hotkey
```bash
provider unbind-head \
  --hotkey=0x1234567890abcdef... \
  --contract=0xContractAddress...
```

---

## 6. Operational Security Guidelines

1. **Never pass tokens via CLI flags**: Avoid passing tokens in process flags (`-jwt=...` or `-pass=...`) where they are visible in `ps aux` or `/proc/<pid>/cmdline`.
2. **File Permissions**: Ensure `~/.urnetwork/jwt` and private keys are set to `0600` permissions.
3. **Container Isolation**: Ensure each container mount uses a dedicated Docker volume (e.g. `ur_config_1`, `ur_config_2`) to prevent state corruption.
