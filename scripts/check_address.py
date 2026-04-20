import os, sys
from eth_account import Account

key = os.environ.get("POLYMARKET_WALLET_KEY")
if not key:
    print("ERROR: POLYMARKET_WALLET_KEY not set")
    sys.exit(1)

if not key.startswith("0x"):
    key = "0x" + key

acct = Account.from_key(key)
print(f"Wallet address: {acct.address}")
print("Compare this to your address on polymarket.com/profile")
