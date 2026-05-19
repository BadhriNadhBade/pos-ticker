# Contributing

Thanks for taking an interest in pos-ticker. Contributions are welcome — bug fixes, new printer support, documentation improvements, anything useful.

## Quick start for development

```bash
git clone https://github.com/badhrinadhbade/pos-ticker.git
cd pos-ticker
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
# fill in at minimum: ADMIN_KEY=dev-key

uvicorn printer_server:app --reload --port 8000
# Admin UI → http://localhost:8000/admin
```

You don't need a physical printer to work on most things. The app starts fine without one — print jobs fail gracefully and are kept in the queue.

## How to contribute

1. **Open an issue first** for anything non-trivial so we can discuss the approach before you spend time on it.
2. Fork the repo and create a branch from `main`:
   ```bash
   git checkout -b fix/your-description
   ```
3. Make your changes. Keep commits focused — one logical change per commit.
4. Open a pull request against `main`. Fill in the PR template.

## What we're looking for

- **Bug fixes** — always welcome, include steps to reproduce in the PR.
- **Printer compatibility** — if your ESC/POS printer needs specific handling, a clean fix for it is great.
- **Security improvements** — see [SECURITY.md](SECURITY.md) for reporting vulnerabilities privately.
- **Documentation** — typo fixes, clearer wording, better examples.

## What we're not looking for (right now)

- Heavy frameworks or new dependencies without a strong reason.
- Abstractions or refactors that don't fix a concrete problem.
- Features that only apply to a very specific setup.

## Code style

- Standard Python — follow the existing style in `printer_server.py`.
- No comments unless the *why* is non-obvious (not the what).
- Keep `printer_server.py` as a single-file server — no splitting into modules unless there's a compelling reason.
- `static/index.html` is intentionally a single file with no build step — keep it that way.

## Running without Docker

The server runs directly with `uvicorn` as shown above. The only system dependency is `libusb` for USB printer access — skip the USB printer setup if you're just working on the API or admin UI.

## Reporting bugs

Open a [GitHub Issue](https://github.com/badhrinadhbade/pos-ticker/issues) using the bug report template. Include your Pi model, OS, Docker version, and what you were doing when it broke.
