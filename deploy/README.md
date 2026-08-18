# Example manifests

These are illustrative copies of the real Kubernetes manifests. The real manifests live in a separate, private repository named `chrisns/infra`. Someone applies them by hand from there. That repository is the only one ever applied to the cluster. See [SPEC.md section 9](../SPEC.md#9-kubernetes) and [VOICE.md section 4](../VOICE.md#4-components) for the authoritative description.

Nothing here is applied automatically. Nothing here is kept in sync automatically. The image tag in `index-note-voice/deployment.yaml` is a point-in-time snapshot. It will drift from whatever is really running. Treat this directory as documentation. It is not a deployable source.

- `index-note/deployment.yaml`: the ingest service. Shown here mainly for the `VOICE_SCORER_URL` line. That line is what wires the two services together.
- `index-note-voice/`: the voice scorer's PVC, Deployment, Service, and kustomization, in full.

No secret material is in these files. Bearer tokens and the ring-hardware prefix live in a gitignored `.env` in the real `chrisns/infra` repository. The manifests read them through `secretKeyRef`. Nothing here writes them out directly.
