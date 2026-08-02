import { useCallback, useEffect, useState } from "react";

/**
 * useOnlineStatus — tracks browser online/offline state.
 *
 * Returns:
 *  - isOnline: current online/offline status from navigator.onLine
 *  - wasOffline: true from the moment offline is first detected
 *    until dismissOffline() is called or the component unmounts
 *  - dismissOffline: resets wasOffline back to false
 */
export function useOnlineStatus(): {
    isOnline: boolean;
    wasOffline: boolean;
    dismissOffline: () => void;
} {
    const [isOnline, setIsOnline] = useState(() =>
        typeof navigator !== "undefined" ? navigator.onLine : true,
    );
    const [wasOffline, setWasOffline] = useState(false);

    useEffect(() => {
        const onOnline = () => {
            setIsOnline(true);
        };
        const onOffline = () => {
            setIsOnline(false);
            setWasOffline(true);
        };

        window.addEventListener("online", onOnline);
        window.addEventListener("offline", onOffline);
        return () => {
            window.removeEventListener("online", onOnline);
            window.removeEventListener("offline", onOffline);
        };
    }, []);

    const dismissOffline = useCallback(() => {
        setWasOffline(false);
    }, []);

    return { isOnline, wasOffline, dismissOffline };
}