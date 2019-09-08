Roger Push Service
==================

Delivers push notifications to APNs via HTTP/2.


Endpoints
---------

### `GET /ping`

Responds with a 200 OK for health checking.


### `POST /v1/push`

Picks up incoming calls and asks the caller to record a message.


Pushing a version
-----------------

```bash
./deploy
```
