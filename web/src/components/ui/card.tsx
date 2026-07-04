import { forwardRef, type HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export interface CardProps extends HTMLAttributes<HTMLDivElement> { }

/** Outer card container. */
export const Card = forwardRef<HTMLDivElement, CardProps>(
    ({ className, ...props }, ref) => (
        <div
            ref={ref}
            className={cn(
                "rounded-xl border border-slate-700/70 bg-surface-raised/80 backdrop-blur",
                "shadow-lg shadow-black/20",
                className,
            )}
            {...props}
        />
    ),
);
Card.displayName = "Card";

export const CardHeader = forwardRef<HTMLDivElement, CardProps>(
    ({ className, ...props }, ref) => (
        <div
            ref={ref}
            className={cn("flex flex-col gap-1 p-5 pb-3", className)}
            {...props}
        />
    ),
);
CardHeader.displayName = "CardHeader";

export const CardTitle = forwardRef<HTMLHeadingElement, HTMLAttributes<HTMLHeadingElement>>(
    ({ className, ...props }, ref) => (
        <h3
            ref={ref}
            className={cn("text-sm font-semibold tracking-wide text-slate-200", className)}
            {...props}
        />
    ),
);
CardTitle.displayName = "CardTitle";

export const CardDescription = forwardRef<
    HTMLParagraphElement,
    HTMLAttributes<HTMLParagraphElement>
>(({ className, ...props }, ref) => (
    <p
        ref={ref}
        className={cn("text-xs text-slate-400", className)}
        {...props}
    />
));
CardDescription.displayName = "CardDescription";

export const CardContent = forwardRef<HTMLDivElement, CardProps>(
    ({ className, ...props }, ref) => (
        <div ref={ref} className={cn("p-5 pt-0", className)} {...props} />
    ),
);
CardContent.displayName = "CardContent";

export const CardFooter = forwardRef<HTMLDivElement, CardProps>(
    ({ className, ...props }, ref) => (
        <div
            ref={ref}
            className={cn("flex items-center p-5 pt-0", className)}
            {...props}
        />
    ),
);
CardFooter.displayName = "CardFooter";
