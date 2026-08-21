"""Bounded input limits for the embedding worker."""

# Allowlisted model aliases — only these are ever accepted.
ALLOWED_ALIASES = frozenset(
    {
        "omahab-embed-english",
        "omahab-embed-worldwide",
    }
)

# Per-request bounds
MAX_BATCH_SIZE = 64
MAX_TEXT_LENGTH = 8192  # chars per chunk; tokenizer may truncate further by tokens
MAX_TOTAL_CHARS_PER_REQUEST = 256 * 1024  # ~256 KiB of text per request
MAX_REQUEST_BYTES = 1_000_000  # ~1 MiB JSON body
MAX_JOB_ID_LENGTH = 128

# Vector bounds
MAX_DIMENSIONS = 4096
MIN_DIMENSIONS = 64

# Transport
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 7700
DEFAULT_SOCKET_PATH = "/run/omahab/embedding.sock"
