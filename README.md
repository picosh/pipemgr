# pipemgr

Send container logs to [pipe](https://pipe.pico.sh) using a docker image.

See [docker-compose.yml](./docker-compose.yml) to see how to run `pipemgr`.

To enable, apply the `pipemgr` label to any service in your `docker-compose.yml`
file:

```yaml
services:
  httpbin:
    image: kennethreitz/httpbin
    label:
      pipemgr.enable: true
```

This will send all stdout/stderr from `httpbin` to `pipe` or any other SSH
service.

## filtering

We support regex filtering with the `pipemgr.filter` label:

```yaml
services:
  httpbin:
    image: kennethreitz/httpbin
    label:
      pipemgr.enable: true
      pipemgr.filter: "GET.+(404)"
```

In this example, we will only send log lines with a GET 404 response status.
