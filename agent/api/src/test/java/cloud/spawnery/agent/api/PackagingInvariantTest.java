/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud.spawnery.agent.api;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.lang.reflect.Constructor;
import java.lang.reflect.Method;
import java.lang.reflect.Modifier;
import java.lang.reflect.ParameterizedType;
import java.lang.reflect.Type;
import java.lang.reflect.TypeVariable;
import java.lang.reflect.WildcardType;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;

/**
 * The rule that makes this module consumable at all, as something that fails.
 *
 * <p>Both agent jars relocate every bundled dependency under
 * {@code cloud.spawnery.agent.shaded.*}. A public signature here carrying a
 * type from anywhere but {@code java.*} or this package would be a signature
 * the shipped jar has moved out from under a plugin compiled against the real
 * one -- a {@code NoSuchMethodError} at the call, with nothing failing at
 * compile time on either side.
 *
 * <p>A build file's comment does not stop a dependency being added. This does.
 */
class PackagingInvariantTest {
    private static final Path CLASSES = Path.of("build/classes/java/main");

    private static boolean permitted(String name) {
        return name.startsWith("java.")
                || name.startsWith("cloud.spawnery.agent.api.");
    }

    @Test
    void everyPublicSignatureUsesOnlyJavaOrThisModulesOwnTypes() throws Exception {
        List<Class<?>> classes = compiledClasses();
        // A scanner that finds nothing passes every assertion below it, which
        // is the one way this test could lie about the thing it exists for.
        assertFalse(
                classes.isEmpty(),
                "no compiled classes under " + CLASSES.toAbsolutePath()
                        + " -- this test found nothing to check and would have passed anyway");

        List<String> offenders = new ArrayList<>();
        for (Class<?> c : classes) {
            for (Method m : c.getDeclaredMethods()) {
                if (!isPublicApi(m.getModifiers())) {
                    continue;
                }
                collect(offenders, c + "." + m.getName() + " returns", m.getGenericReturnType());
                for (Type t : m.getGenericParameterTypes()) {
                    collect(offenders, c + "." + m.getName() + " takes", t);
                }
            }
            for (Constructor<?> k : c.getDeclaredConstructors()) {
                if (!isPublicApi(k.getModifiers())) {
                    continue;
                }
                for (Type t : k.getGenericParameterTypes()) {
                    collect(offenders, c + " constructor takes", t);
                }
            }
        }
        assertTrue(
                offenders.isEmpty(),
                "these public signatures carry a type the agent jars relocate, so a plugin "
                        + "compiled against the real one fails at the call:\n  "
                        + String.join("\n  ", offenders));
    }

    /**
     * No Kotlin anywhere in this module, checked at the annotation rather than
     * at the build file. A Kotlin class carries {@code @kotlin.Metadata}, that
     * annotation is relocated with the rest of the stdlib, and a compiler
     * reading the shipped jar then finds no metadata at all.
     *
     * <p>Unlike its neighbour, this one has not been mutation-checked. Making
     * it fail means adding the Kotlin plugin to this module, which changes
     * dependency resolution and would need agent/deps.json regenerated against
     * a real Maven Central. It is asserted, not measured, and this sentence is
     * here so nobody reads it as the stronger thing.
     */
    @Test
    void nothingHereIsCompiledFromKotlin() throws Exception {
        List<String> kotlinish = new ArrayList<>();
        for (Class<?> c : compiledClasses()) {
            for (var a : c.getDeclaredAnnotations()) {
                if (a.annotationType().getName().startsWith("kotlin")) {
                    kotlinish.add(c.getName() + " carries " + a.annotationType().getName());
                }
            }
        }
        assertTrue(kotlinish.isEmpty(), String.join("\n  ", kotlinish));
    }

    private static boolean isPublicApi(int modifiers) {
        return Modifier.isPublic(modifiers) || Modifier.isProtected(modifiers);
    }

    private static void collect(List<String> into, String where, Type t) {
        if (t instanceof Class<?> c) {
            Class<?> element = c;
            while (element.isArray()) {
                element = element.getComponentType();
            }
            if (element.isPrimitive()) {
                return;
            }
            if (!permitted(element.getName())) {
                into.add(where + " " + element.getName());
            }
            return;
        }
        if (t instanceof ParameterizedType p) {
            collect(into, where, p.getRawType());
            for (Type arg : p.getActualTypeArguments()) {
                collect(into, where, arg);
            }
            return;
        }
        if (t instanceof WildcardType w) {
            for (Type b : w.getUpperBounds()) {
                collect(into, where, b);
            }
            for (Type b : w.getLowerBounds()) {
                collect(into, where, b);
            }
            return;
        }
        if (t instanceof TypeVariable<?> v) {
            for (Type b : v.getBounds()) {
                collect(into, where, b);
            }
        }
    }

    private static List<Class<?>> compiledClasses() throws IOException {
        if (!Files.isDirectory(CLASSES)) {
            return List.of();
        }
        try (Stream<Path> walk = Files.walk(CLASSES)) {
            List<Class<?>> out = new ArrayList<>();
            for (Path p : walk.filter(x -> x.toString().endsWith(".class")).toList()) {
                String name = CLASSES.relativize(p).toString()
                        .replace(java.io.File.separatorChar, '.')
                        .replaceAll("\\.class$", "");
                try {
                    out.add(Class.forName(name, false, PackagingInvariantTest.class.getClassLoader()));
                } catch (ClassNotFoundException e) {
                    throw new AssertionError("compiled class " + name + " is not loadable", e);
                }
            }
            return out;
        }
    }
}
