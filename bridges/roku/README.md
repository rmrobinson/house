# Roku Bridge

This bridge implementation uses the Roku [External Control Protocol](https://developer.roku.com/en-ca/docs/developer-program/dev-tools/external-control-api.md) to discover and retrieve information about Roku devices in the local network.

## TODO
- [ ] update the Roku library to take a context argument to roku.Find()
- [ ] update the Roku library to query [the media player API](https://developer.roku.com/en-ca/docs/developer-program/dev-tools/external-control-api.md#querymedia-player-example) and report the Media object for the Television device, if present