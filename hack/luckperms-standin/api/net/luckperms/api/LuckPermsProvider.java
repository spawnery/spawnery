package net.luckperms.api;

/** As the real provider answers while LuckPerms has not finished, or failed, its enable. */
public final class LuckPermsProvider {
    private LuckPermsProvider() {
    }

    public static LuckPerms get() {
        throw new IllegalStateException("The LuckPerms API isn't loaded yet! (stand-in)");
    }
}
