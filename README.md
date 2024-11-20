# pipemgr

Send container logs to [pipe](https://pipe.pico.sh) using a docker image.

To enable, apply the `pipemgr` label to any service in your `docker-compose.yml` file:

```yaml
services:
  httpbin:
    image: kennethreitz/httpbin
    label:
      pipemgr: true
```

This will send all stdout/stderr from `httpbin` to `pipe` or any other SSH
service.
