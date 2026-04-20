import os
import sys
from py_clob_client.client import ClobClient

key = os.environ.get("POLYMARKET_WALLET_KEY")
if not key:
    print("Error: POLYMARKET_WALLET_KEY env var not set", file=sys.stderr)
    sys.exit(1)

client = ClobClient("https://clob.polymarket.com", chain_id=137, key=key)
creds = client.create_or_derive_api_creds()

print(f"POLYMARKET_API_KEY={creds.api_key}")
print(f"POLYMARKET_API_SECRET={creds.api_secret}")
print(f"POLYMARKET_PASSPHRASE={creds.api_passphrase}")
