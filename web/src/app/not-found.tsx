import Link from "next/link";

export default function NotFound() {
  return (
    <div className="mx-auto max-w-xl px-4 py-24 text-center">
      <div className="eyebrow">404</div>
      <h1 className="display mt-2 text-5xl">Nothing swayed here.</h1>
      <p className="mt-3 text-ink-2">That page doesn&apos;t exist.</p>
      <Link href="/" className="btn btn-primary mt-6">
        Back to Sway
      </Link>
    </div>
  );
}
