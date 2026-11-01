# Token Lab

- `frontend/`: the static page, deployed to Vercel.
- `backend/`: the Go API, deployed to Railway.

## Before going public

1. **Replace the Marvel characters.** Names, sprites and quotes in `frontend/index.html` are for private use. Marvel's characters are protected, and a public site with a donation button is an easy takedown target.
2. Create a **new** Helius API key and delete the old one. The old key was hardcoded in earlier builds and shared in chat.

## 1. Railway (backend)

1. Create a project from this repo and set the service's **Root Directory** to `backend`. `railway.json` sets up the Docker build and the `/healthz` check.
2. Set the variable `HELIUS_API_KEY`.
3. Under Settings > Networking, generate a domain and copy it.

## 2. Vercel (frontend)

1. Import the repo and set **Root Directory** to `frontend`.
2. Set the variable `API_URL` to the Railway address, such as `https://your-api.up.railway.app`.
3. Deploy and copy the Vercel address.

## 3. Connect them

In Railway, set `ALLOWED_ORIGINS` to the Vercel address, such as `https://your-app.vercel.app`. Separate several with commas. Vercel preview addresses are only allowed if you list them.

Open the Vercel address and paste a contract. Jupiter or the X sources may treat cloud addresses differently from your home connection, so watch for rate-limit or failed messages.

## Hosted limits

- There is no password. Anyone can use the site and the API, on your Helius quota, so set a spending limit on your Helius account and watch its usage.
- Each visitor gets 30 lookups a minute, with 4 running at once overall.
- The ATH cache and rate-limit pauses reset whenever the server restarts.
- Auto-run reads the clipboard in the browser, mainly in Chrome and Edge, after asking permission once.

## Local use

Copy `backend/.env.example` to `backend/.env` and add your key, then:

```bash
cd backend && go run . -web -t 120s
```

This serves the page and API at http://127.0.0.1:8787. Don't set `PORT` locally, because that turns on hosted mode.
