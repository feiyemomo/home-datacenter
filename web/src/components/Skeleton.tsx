import { cn } from "@/lib/utils";

interface SkeletonProps {
  className?: string;
}

/**
 * Shimmer skeleton block for loading states.
 * Uses the existing `.shimmer` utility (static under prefers-reduced-motion).
 */
export function Skeleton({ className }: SkeletonProps) {
  return (
    <div
      className={cn(
        "shimmer rounded-xl bg-[rgb(var(--bg-subtle))]",
        className,
      )}
      aria-hidden="true"
    />
  );
}

export default Skeleton;
