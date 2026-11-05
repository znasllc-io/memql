import { useId, useRef, useState, type CSSProperties } from "react";
import { ImageIcon, Trash2 } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { Button } from "../../../../kit";
import { IconButton } from "../../../../kit/IconButton";
import type { UploadProvider } from "../../../../items/upload";

// ONE PICTURE OF THE BRAND: the picture small, and Upload and Remove beside it.
//
// UPLOADED THROUGH THE SHELL'S ONE UPLOAD PATH (`uploads.upload`), the way
// every other OS upload is: the picture the person picked goes to their
// Library, and the kit names it `artifact:<id>`. The engine reads it as them
// and keeps it in the kit as a `data:` picture when the kit is saved; the
// preview reads it the same way, so it shows at once. The field draws the file
// the person picked, read in this browser, because `artifact:` is not a
// picture a browser can draw.
//
// A saved kit's pictures arrive as `data:` pictures and are drawn as they are.
// Only a PNG, a JPEG or a GIF of up to 1 MiB is drawn or taken, which is what a
// kit holds: a larger one is refused at its field before it is uploaded, as the
// engine would refuse it on save. Whether a picture can be read is the
// engine's to say, in the preview and on save.

const TYPES = ["image/png", "image/jpeg", "image/gif"];
/** The most one picture of a kit may be (emaildoc's MaxImageBytes). */
const MAX_PICTURE_BYTES = 1024 * 1024;
const RASTER = /^data:image\/(png|jpeg|gif);base64,/;

/** What every picture on the Brand page shares: the upload port, and the pictures picked here, by kit value. */
export interface BrandPictures {
  uploads: UploadProvider;
  /** `artifact:<id>` -> the picture the person picked for it, as this browser draws it. */
  seen: Readonly<Record<string, string>>;
  saw: (value: string, picture: string) => void;
}

/** The picture the person picked, as this browser can draw it. "" when it cannot be read. */
function readPicture(file: File): Promise<string> {
  return new Promise((resolve) => {
    const reader = new FileReader();
    reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : "");
    reader.onerror = () => resolve("");
    reader.readAsDataURL(file);
  });
}

export function PictureField({ name, value, pictures, onChange, hint, ground }: {
  /** What the picture is, as its controls say it: "logo", "name picture". */
  name: string;
  /** The kit's value: "", a saved `data:` picture, or `artifact:<id>` for one uploaded now. */
  value: string;
  pictures: BrandPictures;
  /** A picture landed (its kit value), or was removed (""). */
  onChange: (value: string) => void;
  /** One line under the field saying what the picture is for. */
  hint?: string;
  /** The brand colour the picture is drawn on in an email: the canvas, or the primary for a reversed logo. No picture, no colour. */
  ground?: string;
}) {
  const input = useRef<HTMLInputElement>(null);
  const row = useRef<HTMLDivElement>(null);
  const problemId = useId();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState("");
  const drawn = RASTER.test(value) ? value : value.startsWith("artifact:") ? pictures.seen[value] ?? "" : "";
  const label = name.charAt(0).toUpperCase() + name.slice(1);

  async function picked(file: File | undefined) {
    if (input.current) input.current.value = "";
    if (file === undefined || busy) return;
    if (!TYPES.includes(file.type)) {
      setProblem("Choose a PNG, JPEG or GIF picture.");
      return;
    }
    if (file.size > MAX_PICTURE_BYTES) {
      setProblem("Choose a picture of 1 MiB or less.");
      return;
    }
    setBusy(true);
    setProblem("");
    try {
      const [landed, picture] = await Promise.all([pictures.uploads.upload(file).done, readPicture(file)]);
      const uploaded = `artifact:${landed.artifactId}`;
      pictures.saw(uploaded, picture);
      onChange(uploaded);
    } catch (failure) {
      setProblem(`The picture wasn't uploaded: ${refusalSentence(failure)}`);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="email-picture-field" data-picture={name}>
      <div className="email-picture" ref={row}>
        <span
          className="email-picture-frame"
          style={ground === undefined || drawn === "" ? undefined : ({ "--email-picture-ground": ground } as CSSProperties)}
        >
          {drawn === "" ? <ImageIcon size={16} aria-hidden /> : <img src={drawn} alt="" />}
        </span>
        <Button busy={busy} busyLabel="Uploading" ariaLabel={`Upload the ${name}`} onClick={() => input.current?.click()}>
          Upload
        </Button>
        {value === "" || busy ? null : (
          <IconButton
            label={`Remove the ${name}`}
            onClick={() => {
              setProblem("");
              // The keyboard stays on the field: Upload is what is left of it.
              row.current?.querySelector<HTMLButtonElement>("button")?.focus();
              onChange("");
            }}
          >
            <Trash2 size={16} aria-hidden />
          </IconButton>
        )}
        <input
          ref={input}
          className="os-sr-only"
          type="file"
          accept={TYPES.join(",")}
          aria-label={`${label} file`}
          aria-describedby={problem === "" ? undefined : problemId}
          tabIndex={-1}
          onChange={(event) => void picked(event.target.files?.[0])}
        />
      </div>
      {hint === undefined ? null : (
        <p className="os-caption">
          {hint}
        </p>
      )}
      {problem === "" ? null : (
        <p id={problemId} className="email-field-problem" role="alert">
          {problem}
        </p>
      )}
    </div>
  );
}
