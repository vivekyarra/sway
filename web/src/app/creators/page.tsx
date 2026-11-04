"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";

export default function CreatorsRedirect() {
  const router = useRouter();
  useEffect(() => router.replace("/creator/"), [router]);
  return null;
}
