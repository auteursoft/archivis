Copies of the files Archivis downloads, kept by auteursoft so that
`archivis setup` and the camera-file tests keep working if an original
disappears or changes. `archivis setup` tries the original first and
this mirror second, and checks every file against its pinned SHA-256
either way. Listed in tools/deps/assets.txt and tools/samples/fetch.sh;
filled and checked weekly by .github/workflows/deps.yml.

Each file stays under its own licence:

- **ONNX Runtime** (`onnxruntime-*`): © Microsoft, MIT License.
  https://github.com/microsoft/onnxruntime
- **InsightFace buffalo_l** (`buffalo_l.zip`): the InsightFace project.
  The models are available for **non-commercial research purposes only**.
  https://github.com/deepinsight/insightface
- **CLIP ViT-B/32 ONNX export** (`clip-ViT-B-32-*.onnx`): OpenAI CLIP,
  MIT License; ONNX export by Jina AI clip-as-service (Apache-2.0).
  https://github.com/openai/CLIP, https://github.com/jina-ai/clip-as-service
- **Camera samples** (test files only):
  - raw.pixls.us files: CC0.
  - rawpy test files (letmaik/rawpy): NEFs are NASA photographs (public
    domain); CR2s are from rawsamples.ch (CC BY-NC-SA 4.0).
  - PhotoPrism samples (photoprism/photoprism, assets/samples): AGPL-3.0
    project.
