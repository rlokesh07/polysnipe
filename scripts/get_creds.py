import os
import sys
from py_clob_client.client import ClobClient

key = os.environ.get("POLYMARKET_WALLET_KEY")
funder = os.environ.get("POLYMARKET_FUNDER_ADDRESS")
if not key:
    print("Error: POLYMARKET_WALLET_KEY env var not set", file=sys.stderr)
    sys.exit(1)
if not funder:
    print("Error: POLYMARKET_FUNDER_ADDRESS env var not set (your polymarket.com/profile address)", file=sys.stderr)
    sys.exit(1)

client = ClobClient(
    "https://clob.polymarket.com",
    chain_id=137,
    key=key,
    signature_type=2,  # POLY_GNOSIS_SAFE
    funder=funder,
)
creds = client.create_or_derive_api_creds()

print(f"POLYMARKET_API_KEY={creds.api_key}")
print(f"POLYMARKET_API_SECRET={creds.api_secret}")
print(f"POLYMARKET_PASSPHRASE={creds.api_passphrase}")
