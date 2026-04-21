"""
Reconstructs the EIP-712 hash for a Go-signed order and recovers the signer.
Paste salt, tokenId, makerAmount, takerAmount, signature from a failing Go debug log as args.
"""
import sys
from eth_account import Account
from py_order_utils.model.order import Order
from py_order_utils.model.signatures import POLY_GNOSIS_SAFE
from py_clob_client.config import get_contract_config
from poly_eip712_structs import make_domain
from eth_utils import keccak

SALT         = int(sys.argv[1])
TOKEN_ID     = sys.argv[2]
MAKER_AMOUNT = sys.argv[3]
TAKER_AMOUNT = sys.argv[4]
SIGNATURE    = sys.argv[5]
MAKER        = "0xa5433BCC345E987e1b7e1D67CB704C7828Dc920d"
SIGNER_ADDR  = "0x7Fa16f54B0C1E86b3539E11AD20e2903dD513652"
SIDE         = 0  # BUY
FEE_RATE_BPS = 1000
NEG_RISK     = False

cfg = get_contract_config(137, NEG_RISK)
domain = make_domain(
    name="Polymarket CTF Exchange",
    version="1",
    chainId=str(137),
    verifyingContract=cfg.exchange,
)

order = Order(
    salt=SALT,
    maker=MAKER,
    signer=SIGNER_ADDR,
    taker="0x0000000000000000000000000000000000000000",
    tokenId=int(TOKEN_ID),
    makerAmount=int(MAKER_AMOUNT),
    takerAmount=int(TAKER_AMOUNT),
    expiration=0,
    nonce=0,
    feeRateBps=FEE_RATE_BPS,
    side=SIDE,
    signatureType=POLY_GNOSIS_SAFE,
)

hash_bytes = keccak(order.signable_bytes(domain=domain))
eip712_hash = "0x" + hash_bytes.hex()
print(f"EIP-712 hash: {eip712_hash}")
print(f"Exchange:     {cfg.exchange}")

recovered = Account.recoverHash(hash_bytes, signature=SIGNATURE)
print(f"Recovered:    {recovered}")
print(f"Expected:     {SIGNER_ADDR}")
print(f"Valid:        {recovered.lower() == SIGNER_ADDR.lower()}")
