# `tlog.kontain.me`

`tlog.kontain.me` is a Docker container image registry implementation that redirects requests to other public image registries.
When it receives a request for an image manifest by mutable tag, it collects the immutable digest associated with that tag, and records it in [Sigstore](https://sigstore.dev)'s transparency log, [Rekor](https://docs.sigstore.dev/rekor/overview).

On subsequent requests for a manifest by tag, it checks Rekor to see if it's seen that tag before, and fails if the previously recorded digest doesn't match the current one.

The effect is **transparently verifiable immutable tags for public images**, for any public image registry, without trusting that the registry actually blocks tag updates.

## How To Use It

Instead of:

```
docker pull alpine:3.16.0
```

Just add `tlog.kontain.me/`:

```
docker pull tlog.kontain.me/alpine:3.16.0
```

Or, in your `Dockerfile`, instead of this:

```
FROM alpine:3.16.0
...
```

Just add `tlog.kontain.me/`:

```
FROM tlog.kontain.me/alpine:3.16.0
...
```

## How It Works

When you pull an image through `tlog.kontain.me`, requests for manifests and blobs by immutable content-addressed digest are simply forwared -- `tlog.kontain.me` doesn't store any data, it just forwards your request to the real registry.

When you pull an image manifest by tag, `tlog.kontain.me` proxies the request from the real registry if it can.
Before it serves the manifest back to you, it notes the manifest's digest as reported by the real registry.

It then queries Rekor to see if there have been any previously reported sightings of your image by tag.
If so, and if the previous records point to the same digest it's about to serve, it serves the request.
If the digest doesn't match, that means someone updated the tag, and the proxied request fails.
If there wasn't a previous record of this image by tag, it writes one in Rekor for next time.

The service runs on [Google Cloud Run](https://cloud.google.com/run), and entries in Rekor contain a keyless signature (using Sigstore's code signing cerificate authority, [Fulcio](https://docs.sigstore.dev/fulcio/overview/)) associated with the service's [service account](https://cloud.google.com/run/docs/configuring/service-accounts).
The instance's service account is `tlogistry@kontaindotme.iam.gserviceaccount.com`.

When a manifest request consults Rekor, informaiton about the associated entry is included in headers in the response:

```
--> GET https://tlog.kontain.me/v2/registry.example.biz/my/image/manifests/v1.2.3

HTTP/2.0 200 OK
...
Tlog-Integratedtime: 2022-06-28T13:03:37Z
Tlog-Logindex: 2787015
Tlog-Uuid: 362f8ecba72f432641632fca55dd510f1efcf89105458562f5d5e828262762b5e1ef276ec6d7a00b
...
```

If the request resulted in a new entry being created in Rekor (i.e., if this was the first time the registry has seen the tag), the `Tlog-First-Seen: true` header is also set in the response.

## Deploying

```
terraform init
terraform apply -var project=[MY-PROJECT]
```

This will build the app with [`ko`](https://github.com/google/ko) and deploy it to your project.

By default it deploys in `us-east4`, but you can change this with `-var region=[MY-REGION]`.

The generated Cloud Run URL will be something like https://tlogistry-blahblah-uk.a.run.app, which you can interact with using:

```
docker pull tlogistry-blahblah-uk.a.run.app/alpine:3.16.0
```

## Frequently Asked Questions

### What about `:latest`?

The `:latest` tag is conventionally updated to point to whatever the "latest" version of an artifact is.

`tlog.kontain.me` doesn't treat `:latest` differently from any other tag -- the first time it's asked to fetch `alpine:latest`, it will record what digest that tag points to, and prevent future requests that would serve different content.

This means that the first time you request any image by `:latest` using `tlog.kontain.me`, that version will be frozen in time.
If `:latest` is updated to point to something else, it will not be able to be pulled through `tlog.kontain.me`, as with all tags.

It is a convenience, but it's also an antipattern if you want reliable, consistent behavior from your container images.

### Aren't I just trusting `tlog.kontain.me` not to mutate my tags / sell my data / mine bitcoin?

Oh you are clever.
_Yes you are._

If you don't want to trust me, you can run an instance of this service yourself.
Each unique instance of the service runs with a unique GCP service account, and only records written by that service account are accepted when considering entries in Rekor.

If you don't want to trust immutable tags at all, I recommend pulling images by content-addressed immutable digests.