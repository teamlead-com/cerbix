import { existsSync, readFileSync } from "node:fs";
import { inflateSync } from "node:zlib";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const here = dirname(fileURLToPath(import.meta.url));

function repositoryDocsRoot(): string {
  const candidates = [
    "/docs",
    resolve(here, "../../../docs"),
    resolve(process.cwd(), "docs"),
    resolve(process.cwd(), "../docs"),
  ];
  const root = candidates.find((candidate) =>
    existsSync(resolve(candidate, "logo.svg")),
  );
  if (!root) {
    throw new Error(`Could not locate repository docs from ${here}`);
  }
  return root;
}

function readAsset(name: string): string {
  return readFileSync(resolve(repositoryDocsRoot(), name), "utf8");
}

function readFavicon(): string {
  return readFileSync(resolve(here, "../../public/favicon.svg"), "utf8");
}

function readRepositoryReadme(): string {
  const candidates = [
    resolve(repositoryDocsRoot(), "../README.md"),
    resolve(here, "../../../README.md"),
    resolve(process.cwd(), "README.md"),
    resolve(process.cwd(), "../README.md"),
    "/README.md",
  ];
  const readme = candidates.find((candidate) => existsSync(candidate));
  if (!readme) {
    throw new Error(`Could not locate repository README from ${here}`);
  }
  return readFileSync(readme, "utf8");
}

function pngHasTransparentPixel(png: Buffer): boolean {
  let offset = 8;
  let width = 0;
  let height = 0;
  const idat: Buffer[] = [];
  while (offset < png.length) {
    const length = png.readUInt32BE(offset);
    const type = png.toString("ascii", offset + 4, offset + 8);
    const data = png.subarray(offset + 8, offset + 8 + length);
    if (type === "IHDR") {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      expect(data[8]).toBe(8);
      expect(data[9]).toBe(6);
    } else if (type === "IDAT") {
      idat.push(data);
    }
    offset += length + 12;
  }

  const stride = width * 4;
  const raw = inflateSync(Buffer.concat(idat));
  let rawOffset = 0;
  let previous = Buffer.alloc(stride);
  for (let y = 0; y < height; y += 1) {
    const filter = raw[rawOffset++];
    const row = Buffer.from(raw.subarray(rawOffset, rawOffset + stride));
    rawOffset += stride;
    for (let x = 0; x < stride; x += 1) {
      const left = x >= 4 ? row[x - 4] : 0;
      const above = previous[x];
      const upperLeft = x >= 4 ? previous[x - 4] : 0;
      if (filter === 1) row[x] = (row[x] + left) & 0xff;
      else if (filter === 2) row[x] = (row[x] + above) & 0xff;
      else if (filter === 3) row[x] = (row[x] + Math.floor((left + above) / 2)) & 0xff;
      else if (filter === 4) {
        const estimate = left + above - upperLeft;
        const leftDistance = Math.abs(estimate - left);
        const aboveDistance = Math.abs(estimate - above);
        const upperLeftDistance = Math.abs(estimate - upperLeft);
        const predictor =
          leftDistance <= aboveDistance && leftDistance <= upperLeftDistance
            ? left
            : aboveDistance <= upperLeftDistance
              ? above
              : upperLeft;
        row[x] = (row[x] + predictor) & 0xff;
      } else {
        expect(filter).toBe(0);
      }
    }
    for (let x = 3; x < stride; x += 4) if (row[x] === 0) return true;
    previous = row;
  }
  return false;
}

type PathAttributes = {
  d: string;
  stroke: string;
  strokeWidth: string;
  strokeLinecap: string;
  strokeLinejoin: string;
};

function pathsIn(svg: string): PathAttributes[] {
  return [...svg.matchAll(/<path\b([^>]*)>/g)].map(([, rawAttributes]) => {
    const attribute = (name: string) =>
      rawAttributes.match(new RegExp(`\\b${name}="([^"]+)"`))?.[1] ?? "";
    return {
      d: attribute("d"),
      stroke: attribute("stroke"),
      strokeWidth: attribute("stroke-width"),
      strokeLinecap: attribute("stroke-linecap"),
      strokeLinejoin: attribute("stroke-linejoin"),
    };
  });
}

function rectFill(svg: string): string {
  return svg.match(/<rect\b[^>]*\bfill="([^"]+)"/)?.[1] ?? "";
}

describe("static Sealed C assets", () => {
  it("keeps canonical paths and stroke geometry identical across SVG assets", () => {
    const light = readAsset("logo.svg");
    const dark = readAsset("logo-dark.svg");
    const favicon = readFavicon();
    const expected = [
      {
        d: "M21.4 9.5a8.5 8.5 0 1 0 .1 12.9",
        stroke: "#FFFFFF",
        strokeWidth: "2.5",
        strokeLinecap: "square",
        strokeLinejoin: "round",
      },
      {
        d: "M19.9 16h4.9",
        stroke: "#FFFFFF",
        strokeWidth: "2.5",
        strokeLinecap: "square",
        strokeLinejoin: "round",
      },
    ];

    expect(pathsIn(light)).toEqual(expected);
    expect(pathsIn(favicon)).toEqual(expected);
    expect(pathsIn(dark)).toEqual(
      expected.map((path) => ({ ...path, stroke: "#0B0B0F" })),
    );
    expect(pathsIn(light)).toEqual(pathsIn(favicon));
    expect(pathsIn(light).map(({ d, strokeWidth, strokeLinecap, strokeLinejoin }) => ({
      d,
      strokeWidth,
      strokeLinecap,
      strokeLinejoin,
    }))).toEqual(
      pathsIn(dark).map(({ d, strokeWidth, strokeLinecap, strokeLinejoin }) => ({
        d,
        strokeWidth,
        strokeLinecap,
        strokeLinejoin,
      })),
    );
  });

  it("uses the approved light, dark, and favicon tile/glyph colors", () => {
    const light = readAsset("logo.svg");
    const dark = readAsset("logo-dark.svg");
    const favicon = readFavicon();

    expect(rectFill(light)).toBe("#5854F2");
    expect(light).toContain('stroke="#FFFFFF"');
    expect(rectFill(dark)).toBe("#7D79FF");
    expect(dark).toContain('stroke="#0B0B0F"');
    expect(rectFill(favicon)).toBe("#5854F2");
    expect(favicon).toContain('stroke="#FFFFFF"');
  });

  it("contains a transparent 256 by 256 RGBA PNG generated for the README", () => {
    const png = readFileSync(resolve(repositoryDocsRoot(), "logo.png"));

    expect([...png.subarray(0, 8)]).toEqual([
      0x89,
      0x50,
      0x4e,
      0x47,
      0x0d,
      0x0a,
      0x1a,
      0x0a,
    ]);
    expect(png.toString("ascii", 12, 16)).toBe("IHDR");
    expect(png.readUInt32BE(16)).toBe(256);
    expect(png.readUInt32BE(20)).toBe(256);
    expect(png[25]).toBe(6);
    expect(pngHasTransparentPixel(png)).toBe(true);
  });

  it("keeps the approved README tagline", () => {
    expect(readRepositoryReadme()).toContain("Reliability you can prove.");
  });
});
