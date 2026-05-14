/*
 * admin-settings-branding.js
 * ==========================
 *
 * Stimulus controllers for /admin/settings.
 *
 *   - color-input    binds an HTML5 color picker to the hex text
 *                    input so changing one updates the other.
 *
 *   - image-upload   handles BOTH the logo (horizontal 3:1) and
 *                    icon (square 1:1) fields. Variant-driven via
 *                    data-image-upload-variant-value="logo" or
 *                    "icon"; the variant picks aspect, output px,
 *                    and crop-stage display dimensions.
 *
 *                    State machine:
 *                      idle    -> show saved-image preview tile +
 *                                 [Upload]/[Replace] + [Remove]
 *                      editor  -> show crop stage with the
 *                                 just-loaded image, zoom slider,
 *                                 [Apply] + [Cancel]
 *                      idle*   -> after Apply, back to idle with
 *                                 the new data URI + preview
 *
 *                    File reads use FileReader.readAsDataURL so the
 *                    image flows through the strict CSP
 *                    (img-src 'self' data:; no blob: needed). All
 *                    cropping/compression is client-side; the form
 *                    POST carries the final data URI in a hidden
 *                    field.
 *
 * Loaded as an ExtraScript on /admin/settings via the admin
 * settings handler. CSP-clean (no inline handlers, no eval).
 */

(function () {
  "use strict";

  if (!window.IdentityStimulus || !window.Stimulus) return;

  const Controller = window.Stimulus.Controller;

  // -------------------------------------------------------------
  // color-input: <input type="color"> <-> hex text input
  // -------------------------------------------------------------
  class ColorInput extends Controller {
    connect() { this.syncFromHex(); }

    syncFromHex() {
      const v = (this.hexTarget.value || "").trim();
      if (/^#?[0-9a-fA-F]{6}$/.test(v)) {
        this.pickerTarget.value = v.charAt(0) === "#" ? v : "#" + v;
      }
    }

    syncFromPicker() {
      this.hexTarget.value = this.pickerTarget.value;
    }
  }
  ColorInput.targets = ["picker", "hex"];

  // -------------------------------------------------------------
  // image-upload: file -> crop stage -> canvas -> data URI
  // -------------------------------------------------------------

  // Output specs per variant. Stage dims come from CSS (.crop-stage--{variant})
  // — the JS just needs to know what each pixel of stage corresponds to in
  // source-image space, so it asks the rendered element for its bounding box.
  const VARIANTS = {
    logo: { outputW: 1024, outputH: 256, sizeCapBytes: 200 * 1024 },
    icon: { outputW: 256, outputH: 256, sizeCapBytes: 150 * 1024 },
  };

  function readFileAsDataURL(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = (e) => resolve(e.target.result);
      reader.onerror = () => reject(new Error("could not read file"));
      reader.readAsDataURL(file);
    });
  }

  function loadDataURL(dataUrl) {
    return new Promise((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve(img);
      img.onerror = () => reject(new Error("decoded image is not a supported format"));
      img.src = dataUrl;
    });
  }

  function dataUriBytes(uri) {
    const comma = uri.indexOf(",");
    return comma < 0 ? uri.length : uri.length - comma - 1;
  }

  function compress(canvas, capBytes) {
    // PNG first (lossless, transparency-preserving). Fall back to JPEG
    // with stepped quality if PNG > cap. Last resort: low-quality JPEG.
    const png = canvas.toDataURL("image/png");
    if (dataUriBytes(png) <= capBytes) return png;
    const qualities = [0.92, 0.85, 0.75, 0.65, 0.5];
    for (let i = 0; i < qualities.length; i++) {
      const jpeg = canvas.toDataURL("image/jpeg", qualities[i]);
      if (dataUriBytes(jpeg) <= capBytes) return jpeg;
    }
    return canvas.toDataURL("image/jpeg", 0.4);
  }

  function pointFrom(ev) {
    if (ev.touches && ev.touches.length) {
      return { x: ev.touches[0].clientX, y: ev.touches[0].clientY };
    }
    return { x: ev.clientX, y: ev.clientY };
  }

  class ImageUpload extends Controller {
    connect() {
      // Read variant + output size from the data attribute the templ
      // helper stamps on the controller root. Default: square icon-style.
      const v = this.element.dataset.imageUploadVariantValue || "icon";
      const spec = VARIANTS[v] || VARIANTS.icon;
      this.outputW = spec.outputW;
      this.outputH = spec.outputH;
      this.sizeCapBytes = spec.sizeCapBytes;

      // Cropper state
      this.imageNatW = 0;
      this.imageNatH = 0;
      this.baseZoom = 1;
      this.zoom = 1;
      this.panX = 0;
      this.panY = 0;
      this.dragging = false;
      this.dragStartX = 0;
      this.dragStartY = 0;

      this._onMove = (ev) => this.dragMove(ev);
      this._onUp = () => this.dragEnd();

      this.renderPreview(this.dataUriTarget.value || "");
    }

    fileChosen(ev) {
      const file = ev.target.files && ev.target.files[0];
      if (!file) {
        if (window.console) console.warn("[image-upload] fileChosen with no file");
        return;
      }
      if (window.console) {
        console.info("[image-upload] file chosen", { name: file.name, type: file.type, size: file.size });
      }
      this.statusTarget.textContent = "Loading image (" + Math.round(file.size / 1024) + "KB)…";

      readFileAsDataURL(file)
        .then((dataUrl) => {
          if (window.console) console.info("[image-upload] file read; decoding image…");
          return loadDataURL(dataUrl);
        })
        .then((img) => {
          if (window.console) {
            console.info("[image-upload] image decoded", img.naturalWidth, "×", img.naturalHeight);
          }
          this.imageNatW = img.naturalWidth;
          this.imageNatH = img.naturalHeight;
          this.imageTarget.src = img.src;
          this.enterEditor();
        })
        .catch((err) => {
          if (window.console) console.error("[image-upload] load failed", err);
          this.statusTarget.textContent = "Could not load image: " + err.message;
        });
    }

    enterEditor() {
      this.idleTarget.hidden = true;
      this.editorTarget.hidden = false;
      this.initFit();
      this.statusTarget.textContent =
        "Drag the image to reposition. Use the slider to zoom. Click Apply when ready.";
    }

    enterIdle() {
      this.editorTarget.hidden = true;
      this.idleTarget.hidden = false;
      this.fileTarget.value = "";
    }

    stageRect() {
      // The crop stage's actual rendered rect in client coords.
      // Used by initFit + drag math. CSS controls the stage size
      // per variant; JS reads it back.
      return this.stageTarget.getBoundingClientRect();
    }

    initFit() {
      // Fit so the image covers the stage entirely with the smaller
      // ratio. baseZoom = max(stageW/natW, stageH/natH) ensures one
      // dimension exactly matches the stage and the other extends.
      const r = this.stageRect();
      this.baseZoom = Math.max(r.width / this.imageNatW, r.height / this.imageNatH);
      this.zoom = this.baseZoom;
      this.zoomTarget.value = "100";
      this.centerImage();
      this.applyTransform();
    }

    centerImage() {
      const r = this.stageRect();
      const dw = this.imageNatW * this.zoom;
      const dh = this.imageNatH * this.zoom;
      this.panX = (r.width - dw) / 2;
      this.panY = (r.height - dh) / 2;
    }

    constrainPan() {
      const r = this.stageRect();
      const dw = this.imageNatW * this.zoom;
      const dh = this.imageNatH * this.zoom;
      this.panX = Math.min(0, Math.max(r.width - dw, this.panX));
      this.panY = Math.min(0, Math.max(r.height - dh, this.panY));
    }

    applyTransform() {
      const img = this.imageTarget;
      img.style.width = (this.imageNatW * this.zoom) + "px";
      img.style.height = (this.imageNatH * this.zoom) + "px";
      img.style.transform = "translate(" + this.panX + "px, " + this.panY + "px)";
    }

    zoomChanged(ev) {
      const factor = parseInt(ev.target.value, 10) / 100;
      const newZoom = this.baseZoom * factor;
      const r = this.stageRect();
      const cx = r.width / 2;
      const cy = r.height / 2;
      const srcX = (cx - this.panX) / this.zoom;
      const srcY = (cy - this.panY) / this.zoom;
      this.zoom = newZoom;
      this.panX = cx - srcX * newZoom;
      this.panY = cy - srcY * newZoom;
      this.constrainPan();
      this.applyTransform();
    }

    dragStart(ev) {
      ev.preventDefault();
      const p = pointFrom(ev);
      this.dragging = true;
      this.dragStartX = p.x - this.panX;
      this.dragStartY = p.y - this.panY;
      document.addEventListener("mousemove", this._onMove);
      document.addEventListener("touchmove", this._onMove, { passive: false });
      document.addEventListener("mouseup", this._onUp);
      document.addEventListener("touchend", this._onUp);
    }

    dragMove(ev) {
      if (!this.dragging) return;
      ev.preventDefault();
      const p = pointFrom(ev);
      this.panX = p.x - this.dragStartX;
      this.panY = p.y - this.dragStartY;
      this.constrainPan();
      this.applyTransform();
    }

    dragEnd() {
      this.dragging = false;
      document.removeEventListener("mousemove", this._onMove);
      document.removeEventListener("touchmove", this._onMove);
      document.removeEventListener("mouseup", this._onUp);
      document.removeEventListener("touchend", this._onUp);
    }

    applyCrop() {
      // Source rect in original-image pixels. Stage's (0,0) maps to
      // source (-panX/zoom, -panY/zoom); stage's full size maps to
      // (stageW/zoom, stageH/zoom) in source space.
      const r = this.stageRect();
      const sx = -this.panX / this.zoom;
      const sy = -this.panY / this.zoom;
      const sw = r.width / this.zoom;
      const sh = r.height / this.zoom;

      const canvas = document.createElement("canvas");
      canvas.width = this.outputW;
      canvas.height = this.outputH;
      const ctx = canvas.getContext("2d");
      ctx.imageSmoothingQuality = "high";
      ctx.drawImage(this.imageTarget, sx, sy, sw, sh, 0, 0, this.outputW, this.outputH);

      const dataUri = compress(canvas, this.sizeCapBytes);
      this.dataUriTarget.value = dataUri;
      this.renderPreview(dataUri);
      this.enterIdle();
      this.statusTarget.textContent =
        "Cropped to " + this.outputW + "×" + this.outputH +
        ", " + Math.round(dataUriBytes(dataUri) / 1024) + "KB. " +
        "Save settings to apply.";
    }

    cancelCrop() {
      this.enterIdle();
      this.statusTarget.textContent = "";
    }

    clear() {
      this.dataUriTarget.value = "";
      this.fileTarget.value = "";
      this.renderPreview("");
      this.statusTarget.textContent = "Removed. Save settings to apply.";
      // Hide the Remove button; only relevant when an image is present.
      if (this.hasRemoveBtnTarget) {
        this.removeBtnTarget.classList.add("hidden");
      }
      this.setUploadLabel("Upload");
      this.enterIdle();
    }

    renderPreview(uri) {
      this.previewTarget.innerHTML = "";
      if (!uri) {
        const empty = document.createElement("div");
        empty.className = "image-upload-empty";
        empty.textContent = "No image";
        this.previewTarget.appendChild(empty);
        this.setUploadLabel("Upload");
        return;
      }
      const img = document.createElement("img");
      img.alt = "Preview";
      img.src = uri;
      this.previewTarget.appendChild(img);
      if (this.hasRemoveBtnTarget) {
        this.removeBtnTarget.classList.remove("hidden");
      }
      this.setUploadLabel("Replace");
    }

    setUploadLabel(text) {
      if (this.hasUploadLabelTarget) {
        this.uploadLabelTarget.textContent = text;
      }
    }
  }
  ImageUpload.targets = [
    "file",
    "preview",
    "dataUri",
    "status",
    "idle",
    "editor",
    "stage",
    "image",
    "zoom",
    "removeBtn",
    "uploadLabel",
  ];

  window.IdentityStimulus.register("color-input", ColorInput);
  window.IdentityStimulus.register("image-upload", ImageUpload);
})();
