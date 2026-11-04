"""Hosts the testnet attesters and a read-only dashboard on Modal.

    make modal-deploy     # builds bin/mergoshi-linux-amd64, then `modal deploy deploy/modal_app.py`

One small always-on container runs `mergoshi serve -attesters`. The three
attester keys come from the Modal secret `mergoshi-attesters-env`, created from
the gitignored .env by `make modal-secret`. The dashboard has no write actions.
"""

import os
import subprocess
from pathlib import Path

import modal

ROOT = Path(__file__).resolve().parents[1]

image = (
    modal.Image.debian_slim()
    .apt_install("ca-certificates")
    .add_local_file(ROOT / "bin" / "mergoshi-linux-amd64", "/app/mergoshi", copy=True)
    .run_commands("chmod +x /app/mergoshi")
    .add_local_dir(ROOT / "sim" / "out", "/app/sim/out")
)

app = modal.App("mergoshi-attesters", image=image)
state = modal.Volume.from_name("mergoshi-attester-state", create_if_missing=True)


@app.function(
    secrets=[modal.Secret.from_name("mergoshi-attesters-env")],
    volumes={"/state": state},
    min_containers=1,
    max_containers=1,
    cpu=0.25,
    memory=256,
    timeout=24 * 3600,
)
@modal.concurrent(max_inputs=100)
@modal.web_server(8080, startup_timeout=60)
def dashboard():
    env = dict(os.environ)
    env.update({
        "MERGOSHI_ENV": "/nonexistent.env",  # settings come from the secret only
        "MERGOSHI_STATE_DIR": "/state",
        "MERGOSHI_SIM_DIR": "/app/sim/out",
        # A gentle pace for the shared, unauthenticated GitHub limit (60 calls an hour):
        # one attester searches for one new merge every 8 minutes; all three answer
        # open rounds every 20 seconds so no commit window is missed.
        "MERGOSHI_DISCOVER_FIRST_ONLY": "1",
        "MERGOSHI_DISCOVER_EVERY": "8m",
        "MERGOSHI_MAX_PER_TICK": "1",
        "MERGOSHI_ATTEST_EVERY": "20s",
    })
    subprocess.Popen(["/app/mergoshi", "serve", "-attesters", "-addr", ":8080"], cwd="/app", env=env)
