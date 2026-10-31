This directory is embedded into the relay binary. `mise run relay:build` fills it
with the built website (apps/frontend/dist), so the relay can serve bootlet at
http://localhost:8089. Everything here except this file is generated.
